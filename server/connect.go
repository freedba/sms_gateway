package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenhg5/collection"
	cmap "github.com/orcaman/concurrent-map"
	"io"
	"net"
	"regexp"
	"runtime"
	"sms_lib/config"
	"sms_lib/levellogger"
	"sms_lib/models"
	"sms_lib/protocol/cmpp"
	"sms_lib/protocol/common"
	"sms_lib/protocol/socket"
	"sms_lib/utils"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type SrvConn struct {
	conn          net.Conn
	Account       AccountsInfo
	Logger        *levellogger.Logger
	rw            *socket.PacketRW
	waitGroup     utils.WaitGroupWrapper
	terminateSent int32

	addr       string
	RemoteAddr string
	RunId      string

	ReadLoopRunning   int32
	deliverSenderExit int32
	CloseFlag         int32

	longSms *LongSmsMap
	lsLock  *sync.Mutex
	lsmLock *sync.Mutex

	deliverMsgMap         cmap.ConcurrentMap
	deliverResendCountMap cmap.ConcurrentMap

	ExitSrv         chan struct{}
	hsmChan         chan HttpSubmitMessageInfo
	mapKeyInChan    chan string
	SubmitChan      chan *cmpp.Submit
	deliverRespChan chan cmpp.DeliverResp
	commandChan     chan []byte
	deliverFakeChan chan []byte
	ATRespSeqId     chan uint32

	FlowVelocity *utils.FlowVelocity
	RateLimit    *utils.RateLimit

	SubmitToQueueCount  int64
	DeliverSendCount    int
	SeqId               uint32
	LastSeqId           uint32
	invalidMessageCount uint32
	activeTestCount     uint32
	submitTaskCount     int64
	deliverTaskCount    int64
	MsgId               uint64
	count               uint64
}

func Listen(sess *Sessions) {
	var mLock = &sync.Mutex{}
	sess.mLock = mLock

	addr := config.GetListen()
	if addr == "" {
		addr = "0.0.0.0:7890"
	}
	logger.Debug().Msgf("start Server at %s", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error().Msgf("error listening: %v", err.Error())
		return
	}

	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			logger.Error().Msgf("ln.Close error: %v", err)
		}
	}(ln)

	sess.ln = ln
	go signalHandle(sess)

	for {
		clientConn, err := ln.Accept()
		if err != nil {
			logger.Error().Msgf("error Listen Accept: %v", err.Error())
			if nErr, ok := err.(net.Error); ok && nErr.Timeout() {
				logger.Error().Msgf("NOTICE: Timeout Accept() failure - %s", err)
				runtime.Gosched()
				continue
			}
			if !strings.Contains(err.Error(), "use of closed network connection") {
				logger.Error().Msgf("ERROR: listener.Accept() - %s", err)
			}
			return
		}

		tc := clientConn.(*net.TCPConn)
		_ = tc.SetKeepAlive(true)

		sess.RemoteAddr = strings.Split(tc.RemoteAddr().String(), ":")[0]
		logger.Debug().Msgf("New connection：%v, Peer ip ：%s", clientConn, sess.RemoteAddr)

		go HandleNewConn(clientConn, sess)
	}
}

func HandleNewConn(conn net.Conn, sess *Sessions) {
	var resp *cmpp.ConnResp
	var err error
	var runId string
	var data []byte
	//var strPoint string

	qLen := utils.GetAttr("qlen")
	logger.Info().Msgf("队列缓冲值: %d", qLen)

	s := &SrvConn{
		conn:                  conn,
		RemoteAddr:            sess.RemoteAddr,
		SubmitChan:            make(chan *cmpp.Submit, qLen),
		commandChan:           make(chan []byte, qLen),
		hsmChan:               make(chan HttpSubmitMessageInfo, qLen),
		deliverRespChan:       make(chan cmpp.DeliverResp, qLen),
		mapKeyInChan:          make(chan string, qLen),
		ExitSrv:               make(chan struct{}),
		ATRespSeqId:           make(chan uint32, 1),
		deliverMsgMap:         cmap.New(),
		deliverResendCountMap: cmap.New(),
		rw:                    socket.NewPacketRW(conn),
		longSms: &LongSmsMap{
			LongSms: make(map[uint8]*LongSms),
			mLock:   new(sync.Mutex),
		},
		lsLock:  new(sync.Mutex),
		lsmLock: new(sync.Mutex),
	}
	if FakeGateway == 1 {
		s.deliverFakeChan = make(chan []byte, qLen)
	}
	h := &cmpp.Header{}

	s.rw.ReadTimeout = 10
	data, err = s.rw.ReadPacket()
	if err != nil {
		logger.Error().Msgf("s.rw.ReadPacket error:%v,read len:%d", err, len(data))
		goto EXIT
	}
	s.rw.Reset("r")

	h.UnPack(data[:12])
	if h.CmdId != common.CMPP_CONNECT {
		logger.Debug().Msgf("CmdId is not CMPP_CONNECT: %v", h)
		goto EXIT
	}

	resp, err = s.NewAuth(data[12:], sess)
	if err != nil {
		logger.Error().Msgf("s.NewAuth error: %v", err)
		//goto EXIT
	}

	s.rw.WriteTimeout = 10
	if err = resp.IOWrite(s.rw); err != nil {
		logger.Error().Msgf("resp.IOWrite error: %v", err)
		goto EXIT
	}
	s.rw.Reset("w")

	if resp.Status != 0 {
		time.Sleep(time.Duration(1) * time.Second)
		goto EXIT
	}

	sess.Add(s.Account.NickName, s)
	runId = s.Account.NickName + ":" + fmt.Sprintf("%p", s.conn)
	s.Logger = levellogger.NewLogger(runId)
	s.RunId = runId
	s.rw.RunId = runId
	//InitChan(runId) //go通道缓冲初始化

	s.waitGroup.Wrap(func() { s.ReadLoop() })
	time.Sleep(time.Duration(10) * time.Millisecond)
	s.waitGroup.Wrap(func() { SubmitMsgIdToQueue(s) })
	s.waitGroup.Wrap(func() { DeliverPush(s) })

	s.waitGroup.Wait()
	sess.Done(s.Account.NickName, s)
EXIT:
	s.Close()
	logger.Debug().Msgf("socket %v, Exiting HandleNewConn...", s.conn)
}

func (s *SrvConn) NewAuth(buf []byte, sess *Sessions) (*cmpp.ConnResp, error) {
	var err error

	req := &cmpp.ConnReq{}
	req.UnPack(buf)

	resp := cmpp.NewConnResp()
	resp.SeqId = req.SeqId
	resp.TotalLen = common.CMPP_HEADER_SIZE + 1 + 16 + 1
	resp.Status = 0
	sourceAddr := req.SourceAddr
	user := sourceAddr.String()

	if len(buf) != 27 {
		err = common.ConnectRspResultErrMap[common.ErrnoConnectInvalidStruct]
		logger.Error().Msgf("len(buf) != 27 error:%v", err)
		resp.Status = common.ErrnoConnectInvalidStruct
		return nil, err
	}

	rKey := "index:user:userinfo:"
	str := models.RedisHGet(rKey, user)
	if str == "" {
		err = common.ConnectRspResultErrMap[common.ErrnoConnectAuthFaild]
		logger.Error().Msgf("账号(%s) 不存在, error:%v", user, err)
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	var account AccountsInfo
	account = AccountsInfo{}
	err = json.Unmarshal([]byte(str), &account)
	if err != nil {
		logger.Error().Msgf("accounts json.unmarshal error:%v, exit...", err)
		err = common.ConnectRspResultErrMap[common.ErrnoConnectInvalidStruct]
		resp.Status = common.ErrnoConnectInvalidStruct
		return resp, err
	}
	logger.Debug().Msgf("account:%+v", account)

	if account.FlowVelocity == 0 {
		err = common.ConnectRspResultErrMap[common.ErrnoConnectAuthFaild]
		logger.Error().Msgf("账号(%s) 流控为0，拒绝建立连接, error:%v", user, err)
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	if account.ConnFlowVelocity == 0 {
		account.ConnFlowVelocity = 100
	}

	currConns := sess.GetUserConn(user)
	maxConns := account.FlowVelocity / account.ConnFlowVelocity
	//isLogin, ok := sess.Users[user]
	//if ok && len(isLogin) == 5 {
	if currConns+1 > maxConns {
		logger.Error().Msgf("账号(%s) 当前建立的连接数已达最大允许连接数:%d", user, maxConns)
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	//验证远程登录地址是否在白名单中
	whiteList := strings.Split(account.AccountHost, ",")
	if !collection.Collect(whiteList).Contains(sess.RemoteAddr) {
		logger.Error().Msgf("账号(%s) 登录ip地址(%s)非法!", sourceAddr.String(), sess.RemoteAddr)
		err = common.ConnectRspResultErrMap[common.ErrnoConnectInvalidSrcAddr]
		resp.Status = common.ErrnoConnectInvalidSrcAddr
		return resp, err
	}

	authSrc, err := common.GenAuthSrc(req.SourceAddr.String(), account.CmppPassword, req.Timestamp, 9)
	if err != nil {
		logger.Error().Msgf("通道(%s) 生成authSrc信息错误：%v", sourceAddr.String(), err, authSrc)
		err = common.ConnectRspResultErrMap[common.ErrnoConnectAuthFaild]
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	reqAuthSrc := req.AuthSrc.Byte()
	if !bytes.Equal(authSrc, reqAuthSrc) {
		logger.Error().Msgf("账号(%s) 密码不匹配，auth failed", sourceAddr.String())
		logger.Error().Msgf("authSrc: %v, req.AuthSrc: %v", authSrc, reqAuthSrc)
		err = common.ConnectRspResultErrMap[common.ErrnoConnectAuthFaild]
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	authImsg, err := common.GenRespAuthSrc(resp.Status, string(authSrc), account.CmppPassword)
	if err != nil {
		logger.Debug().Msgf("通道(%s) 生成authImsg信息错误：%v", sourceAddr.String(), err, authImsg)
		err = common.ConnectRspResultErrMap[common.ErrnoConnectAuthFaild]
		resp.Status = common.ErrnoConnectAuthFaild
		return resp, err
	}

	resp.AuthIsmg = &common.OctetString{
		Data:     authImsg,
		FixedLen: 16,
	}

	logger.Debug().Msgf("账号(%s) 登录成功，远程地址：%s", req.SourceAddr, s.RemoteAddr)
	s.Logger = levellogger.NewLogger(strconv.Itoa(int(s.Account.Id)))
	s.Account = account
	return resp, nil
}

func (s *SrvConn) CloseConnection() {
	if err := s.conn.Close(); err != nil {
		logger.Error().Msgf("通道(%s) conn Unexpected close error: %v", s.RunId, err)
	}
}

func (s *SrvConn) IsClosing() bool {
	return atomic.LoadInt32(&s.CloseFlag) == 1
}

func (s *SrvConn) Close() {
	runId := s.RunId
	if s.IsClosing() {
		logger.Debug().Msgf("帐号(%s) Connection has closed, Peer ip: %s", runId, s.RemoteAddr)
		return
	}
	atomic.StoreInt32(&(s.CloseFlag), 1)
	logger.Debug().Msgf("帐号(%s) conn will be closed: %v, Peer ip: %s", runId, s.conn, s.RemoteAddr)
	s.CloseConnection()
	logger.Debug().Msgf("帐号(%s) Connection closed, Peer ip: %s", runId, s.RemoteAddr)
}

func (s *SrvConn) Cleanup() {
	runId := s.RunId
	s.Logger.Debug().Msgf("帐号(%s) 关闭网络连接，并清理缓存中的数据", runId)

	for {
		t1 := len(s.mapKeyInChan)
		if t1 == 0 {
			break
		} else {
			select {
			case mapKey := <-s.mapKeyInChan: // 清理回执发送控制缓冲队列
				s.Logger.Debug().Msgf("帐号(%s) record Deliver mapKey：%s", runId, mapKey)
				if _, ok := s.deliverMsgMap.Get(mapKey); ok {
					s.deliverMsgMap.Remove(mapKey)
				}
			default:
			}
		}
	}
	s.Logger.Debug().Msgf("帐号(%s) 缓存数据已清理，Exiting Cleanup...", runId)
}

func (s *SrvConn) loopMakeMsgId(ctx context.Context) {
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	timeout := time.Duration(2) * time.Second
	msgIdChan = make(chan uint64, 10000)
	for {
		utils.ResetTimer(timer, timeout)
		select {
		case <-ctx.Done():
			return
		case msgIdChan <- GenerateMsgID():
		case <-timer.C:
		}
	}
}

func (s *SrvConn) ReadLoop() {
	atomic.StoreInt32(&s.ReadLoopRunning, 1)
	timer := time.NewTimer(utils.Timeout)
	runId := s.RunId
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { s.HandleCommand(ctx) })
	s.waitGroup.Wrap(func() { s.loopMakeMsgId(ctx) })
	s.waitGroup.Wrap(func() { s.LoopActiveTest(ctx) })

	for {
		data, err := s.rw.ReadPacket()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				time.Sleep(time.Duration(1) * time.Second)
				s.Logger.Error().Msgf("通道(%s) IO Timeout - %s",
					runId, err)
				continue
			}
			if err == io.EOF {
				s.Logger.Error().Msgf("通道(%s) IO EOF error %s, s.IsClosing:%v", runId, err, s.IsClosing())
				goto EXIT
			}
			if !strings.Contains(err.Error(), "use of closed network connection") {
				s.Logger.Error().Msgf("通道(%s) IO error - %s", runId, err)
			}
			goto EXIT
		}
		utils.ResetTimer(timer, utils.Timeout)

		select {
		case s.commandChan <- data:
			s.invalidMessageCount = 0
		case <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) 数据写入管道 s.commandChan 超时,s.commandChan len: %d",
				runId, len(s.commandChan))
			s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", runId, data)
		}
	}
EXIT:
	atomic.StoreInt32(&s.ReadLoopRunning, 0)
	s.waitGroup.Wrap(func() { s.Cleanup() })
	s.Logger.Debug().Msgf("账号(%s) Exiting ReadLoop...", runId)
}

func (s *SrvConn) HandleCommand(ctx context.Context) {
	runId := s.RunId
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	if s.Account.ConnFlowVelocity == 0 {
		s.Account.ConnFlowVelocity = 300
	}

	var rateLimit = utils.NewRateLimit(s.Account.ConnFlowVelocity, runId, runMode)
	s.RateLimit = rateLimit

	s.Logger.Debug().Msgf("账号(%s) 启动 HandleCommand 协程", runId)
	h := &cmpp.Header{}
	r := cmpp.NewActiveTestResp()

	for {
		select {
		case <-s.ExitSrv:
			s.Logger.Debug().Msgf("账号(%s) 接收到 s.ExitSrv 退出信号, 退出 HandleCommand 协程....", runId)
			if atomic.LoadInt32(&s.terminateSent) == 0 {
				t := cmpp.NewTerminate()
				_ = t.IOWrite(s.rw)
				atomic.StoreInt32(&s.terminateSent, 1)
				time.Sleep(time.Duration(1) * time.Second)
				if !s.IsClosing() {
					s.Close() //关闭网络读写
				}
			}
		default:
		}

		utils.ResetTimer(timer, utils.Timeout)
		select {
		case data := <-s.commandChan:
			h.UnPack(data[:common.CMPP_HEADER_SIZE])
			switch h.CmdId {
			case common.CMPP_ACTIVE_TEST:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试命令(CMPP_ACTIVE_TEST), SeqId: %d", runId, h.SeqId)
				r.SeqId = h.SeqId
				if err := r.IOWrite(s.rw); err != nil {
					s.Logger.Error().Msgf("发送数据失败,error:%v", err)
					s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runId)
					if !utils.ChIsClosed(s.ExitSrv) {
						close(s.ExitSrv)
					}
				}
				atomic.StoreUint32(&s.activeTestCount, 0)

			case common.CMPP_ACTIVE_TEST_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试应答命令(CMPP_ACTIVE_TEST_RESP), SeqId: %d", runId, h.SeqId)
				select {
				case s.ATRespSeqId <- h.SeqId:
				case <-timer.C:
				}

			case common.CMPP_TERMINATE:
				s.Logger.Warn().Msgf("账号(%s) 收到拆除连接命令(CMPP_TERMINATE), SeqId:%d", runId, h.SeqId)
				s.Logger.Info().Msgf("账号(%s) 发送拆除连接应答命令(CMPP_TERMINATE_RESP)", runId)
				if err := cmpp.NewTerminateResp().IOWrite(s.rw); err != nil { //拆除连接
					s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE_RESP IOWrite: %v", runId, err)
				}
				goto EXIT

			case common.CMPP_TERMINATE_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到拆除连接应答命令(CMPP_TERMINATE_RESP), SeqId:%d", runId, h.SeqId)
				goto EXIT

			case common.CMPP_SUBMIT:
				count := atomic.AddInt64(&s.submitTaskCount, 1)
				if int(count) > 50 {
					s.Logger.Warn().Msgf("账号(%s) s.submitTaskCount: %d, sleep 100ms", runId, count)
					time.Sleep(time.Duration(100) * time.Millisecond)
				}
				s.waitGroup.Wrap(func() { s.handleSubmit(data) })

			case common.CMPP_DELIVER_RESP:
				atomic.AddInt64(&s.deliverTaskCount, 1)
				s.waitGroup.Wrap(func() { s.handleDeliverResp(data) })

			default:
				s.Logger.Debug().Msgf("账号(%v) 命令未知, %v\n", runId, *h)
				// time.Sleep(time.Duration(1000) * time.Nanosecond)
			}
		case <-timer.C:
			//s.Logger.Debug().Msgf("账号(%s) HandleCommand Tick at", runId)
			select {
			case <-ctx.Done():
				s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号, 退出 HandleCommand 协程....", runId)
				goto EXIT
			default:
			}
		}
	}
EXIT:
	if !s.IsClosing() {
		s.Close() //关闭网络读写
	}
	s.Logger.Debug().Msgf("账号(%s) Exiting HandleCommand...", runId)
}

func (s *SrvConn) handleSubmit(data []byte) {
	var count uint64
	var err error
	h := &cmpp.Header{}
	h.UnPack(data[:common.CMPP_HEADER_SIZE])
	buf := data[common.CMPP_HEADER_SIZE:]
	runId := s.RunId
	resp := cmpp.NewSubmitResp()
	p := &cmpp.Submit{}
	resp.MsgId = <-msgIdChan
	if resp.MsgId == 0 {
		s.Logger.Error().Msgf("msgId generate error:")
		s.Logger.Error().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE)", runId)
		goto EXIT
	}
	resp.SeqId = h.SeqId
	count = atomic.AddUint64(&s.count, 1)

	if !s.RateLimit.Available() {
		s.Logger.Error().Msgf("账号(%s) 流速控制触发：%d", runId, s.FlowVelocity.Rate)
		resp.Result = 8
	} else if len(buf)+12 != int(h.TotalLen) {
		s.Logger.Error().Msgf("账号(%s) buf len:%d + 12 != h.TotalLen:%d", runId, len(buf), h.TotalLen)
		resp.Result = common.ErrnoSubmitInvalidMsgLength
	} else if s.LastSeqId == h.SeqId && s.LastSeqId != 0 {
		s.Logger.Error().Msgf("账号(%s) s.LastSeqId(%d) == h.SeqId(%d) && s.LastSeqId != 0",
			runId, s.LastSeqId, h.SeqId)
		resp.Result = common.ErrnoSubmitInvalidSequence
	} else {
		if err := p.UnPack(buf); err != nil {
			s.Logger.Error().Msgf("账号(%s) p.UnPack(buf) error:%v", runId, err)
			resp.Result = common.ErrnoSubmitInvalidStruct
		}
	}

	if resp.Result == 0 {
		p.SeqId = h.SeqId
		p.MsgId = resp.MsgId
		resp.Result = s.VerifySubmit(p)
	}

	if err = resp.IOWrite(s.rw); err != nil {
		s.Logger.Error().Msgf("账号(%s) SubmitResp IOWrite error: %v", runId, err)
		goto EXIT
	}

	if count%uint64(utils.PeekInterval) == 0 {
		s.Logger.Debug().Msgf("账号(%s) 收到消息提交(CMPP_SUBMIT), count: %d,resp.SeqId:%d,resp.MsgId:%d,"+
			"s.submitTaskCount:%d", runId, s.count, resp.SeqId, resp.MsgId, atomic.LoadInt64(&s.submitTaskCount))
	}

	s.LastSeqId = h.SeqId
	if resp.Result == 0 {
		if FakeGateway == 1 { //模拟网关服务器
			go s.makeDeliverMsg(p.MsgId, p.DestTerminalId)
		} else {
			timer := time.NewTimer(utils.Timeout * 10)
			select {
			case s.SubmitChan <- p:
			case t := <-timer.C:
				s.Logger.Debug().Msgf("账号(%s) 写入管道 s.SubmitChan 超时, Tick at: %v", runId, t)
				s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", s.RunId, p)
			}
		}
	} else {
		if resp.Result == common.ErrnoSubmitInvalidMsgLength ||
			resp.Result == common.ErrnoSubmitInvalidSequence ||
			resp.Result == common.ErrnoSubmitInvalidStruct ||
			resp.Result == common.ErrnoSubmitInvalidSrcId ||
			resp.Result == common.ErrnoSubmitInvalidMsgSrc ||
			resp.Result == common.ErrnoDeliverInvalidServiceId {
			s.Logger.Warn().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE),resp.Result:%d", runId, resp.Result)
			if err := cmpp.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
				s.Logger.Error().Msgf("账号(%s) Terminate IOWrite error: %v", runId, err)
			}
			time.Sleep(time.Duration(1) * time.Second)
			goto EXIT
		}
	}
	atomic.AddInt64(&s.submitTaskCount, -1)
	return
EXIT:
	s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runId)
	if !utils.ChIsClosed(s.ExitSrv) {
		close(s.ExitSrv)
	}
	atomic.AddInt64(&s.submitTaskCount, -1)
}

func (s *SrvConn) VerifySubmit(p *cmpp.Submit) uint8 {
	runId := s.RunId

	if p.RegisteredDelivery < 0 && p.RegisteredDelivery > 2 {
		logger.Error().Msgf("账号(%s) 提交的信息:p.RegisteredDelivery != 0-2", runId)
		return common.ErrnoSubmitInvalidStruct
	}
	if p.ServiceId.String() != s.Account.NickName {
		logger.Error().Msgf("账号(%s) 提交的信息:s.Account.NickName != p.ServiceId.String()", runId)
		return common.ErrnoDeliverInvalidServiceId
	}

	if p.MsgSrc.String() != s.Account.NickName {
		logger.Error().Msgf("账号(%s) 提交的信息:s.Account.NickName != p.ServiceId.String()", runId)
		return common.ErrnoDeliverInvalidStruct
	}

	if p.TPUdhi == 0 {

	} else if p.TPUdhi == 1 { //长短信检验
		udhi := p.MsgContent[0:6]
		rand := udhi[3]
		msgId := strconv.FormatUint(p.MsgId, 10)
		pkTotal := p.PkTotal
		pkNumber := p.PkNumber
		s.lsLock.Lock()
		if !s.longSms.exist(rand) {
			ls := &LongSms{
				Content: make(map[uint8][]byte),
				MsgID:   make(map[uint8]string),
				mLock:   new(sync.Mutex),
			}
			s.longSms.set(rand, ls)
		}
		ls := s.longSms.get(rand)
		if ls.exist(pkNumber) {
			s.Logger.Error().Msgf("账号(%s) pkTotal:%d, pkNumber:%d, rand:%d, msgID string:%s, msgID:%d 长短信标志位重复",
				s.RunId, pkTotal, pkNumber, rand, msgId, p.MsgId)
			return common.ErrnoSubmitInvalidStruct
		}
		ls.set(pkNumber, msgId, p.MsgContent[6:])
		s.lsLock.Unlock()
		if utils.Debug {
			s.Logger.Error().Msgf("账号(%s) pkTotal:%d, pkNumber:%d, rand:%d, msgID string:%s, msgID:%d, s.longSms len:%d, s.longSms:%+v",
				s.RunId, pkTotal, pkNumber, rand, msgId, p.MsgId, s.longSms.len(), s.longSms.LongSms)
		}
	} else {
		s.Logger.Error().Msgf("账号(%s) 提交的信息TPPid > 1", runId)
		return common.ErrnoSubmitInvalidStruct
	}

	regexpStr := "^" + p.SrcId.String()
	re := regexp.MustCompile(s.Account.CmppDestId)
	if !re.MatchString(regexpStr) {
		logger.Error().Msgf("账号(%s) 提交的信息:s.Account.CmppDestId !~ ^p.SrcId.String()", runId)
		return common.ErrnoSubmitInvalidSrcId
	}

	if len(p.SrcId.String()) < len(s.Account.CmppDestId) {
		logger.Error().Msgf("账号(%s) 提交的信息:len(p.SrcId.String()) < len(s.Account.CmppDestId)", runId)
		return common.ErrnoSubmitInvalidSrcId
	}
	return 0
}

func (s *SrvConn) handleDeliverResp(data []byte) {
	h := &cmpp.Header{}
	h.UnPack(data[:common.CMPP_HEADER_SIZE])
	buf := data[common.CMPP_HEADER_SIZE:]
	runId := s.RunId
	dr := &cmpp.DeliverResp{}
	dr.UnPack(buf)
	dr.SeqId = h.SeqId
	if dr.Result != 0 {
		s.Logger.Error().Msgf("账号(%s) 接收到的 CMPP_DELIVER_RESP Result: %d, msgId: %d",
			runId, dr.Result, dr.MsgId)
	}
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	select {
	case s.deliverRespChan <- *dr:
	case t := <-timer.C:
		s.Logger.Debug().Msgf("账号(%s) 写入管道 s.deliverRespChan 超时, Tick at: %v", runId, t)
	}
	atomic.AddInt64(&s.deliverTaskCount, -1)
}

func (s *SrvConn) LoopActiveTest(ctx context.Context) {
	//通信双方以客户-服务器方式建立 TCP 连接，用于双方信息的相互提交。当信道上没有数据
	//传输时，通信双方应每隔时间 C 发送链路检测包以维持此连接，当链路检测包发出超过时
	//间 T 后未收到响应，应立即再发送链路检测包，再连续发送 N-1 次后仍未得到响应则断开
	//此连接
	// c=60s, t=10, n=3
	var timer1 = 0  // 对端发送CMPP_ACTIVE_TEST超时计时
	var sendTry = 0 //发送CMPP_ACTIVE_TEST到对端尝试次数，max=3
	var count = 0

	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	runId := s.RunId
	hbTime := config.GetHBTime()
	timeout := hbTime.Timeout
	retry := hbTime.Retry
	tick := int(time.Duration(timeout) * time.Second / utils.Timeout)
	if tick == 0 {
		tick = 1
	}

	p := cmpp.NewActiveTest()
	p.SeqId = atomic.LoadUint32(&s.SeqId)
	s.Logger.Debug().Msgf("账号(%s) 启动 LoopActiveTest", runId)
	for {
		if atomic.LoadInt32(&s.ReadLoopRunning) == 0 {
			s.Logger.Debug().Msgf("通道(%s) c.ReadLoopRunning: 0， 退出LoopActiveTest", runId)
			goto EXIT
		}

		utils.ResetTimer(timer, utils.Timeout)
		select {
		case <-s.ATRespSeqId:
			//c.Logger.Debug().Msgf("账号(%s)接收到心跳应答包(CMPP_ACTIVE_TEST_RESP),RespSeqId: %d, timer1: %d, timer2: %d", chid, RespSeqId, timer1,timer2)
			sendTry = 0
		default:
		}

		select {
		//case <-utils.NotifySig.IsTransfer[runId]: //通信信道上有数据不发送检测包
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到ctx.Done信号，退出LoopActiveTest", runId)
			goto EXIT
		case <-timer.C:
		}

		if sendTry < retry && count%tick == 0 {
			p.SeqId = atomic.AddUint32(&s.SeqId, 1)
			//s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST), seqId: %d", runId, p.SeqId)
			if err := p.IOWrite(s.rw); err != nil {
				//s.Logger.Error().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST)失败:%v", runId, err)
				timer1 = tick + 1
				sendTry = 3
			}
			sendTry++
		}
		if timer1 > tick && sendTry >= retry {
			s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST) send try: %d and "+
				"接收心跳包命令(CMPP_ACTIVE_TEST_RESP) timeout: %d, will exit ", runId, sendTry, timer1)
			s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runId)
			if !utils.ChIsClosed(s.ExitSrv) {
				close(s.ExitSrv)
			}
		}
		count++
		timer1 = int(atomic.AddUint32(&s.activeTestCount, 1))

	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting LoopActiveTest...", runId)
}
