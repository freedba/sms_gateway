package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

	"github.com/chenhg5/collection"
	cmap "github.com/orcaman/concurrent-map/v2"
	"golang.org/x/exp/slices"
)

type SrvConn struct {
	conn          net.Conn
	Account       *AccountsInfo
	Logger        *levellogger.Logger
	rw            *socket.PacketRW
	waitGroup     utils.WaitGroupWrapper
	terminateSent int32
	mutex         *sync.Mutex
	BsiLock       *sync.RWMutex

	addr       string
	RemoteAddr string
	RunID      string

	ReadLoopRunning   int32
	deliverSenderExit int32
	CloseFlag         int32

	lsm     *LongSmsMap
	lsLock  *sync.Mutex
	lsmLock *sync.Mutex

	deliverMsgMap cmap.ConcurrentMap[string, *deliverWithTime]

	ExitSrv         chan struct{}
	hsmChan         chan HTTPSubmitMessageInfo
	mapKeyInChan    chan string
	SubmitChan      chan *cmpp.Submit
	deliverRespChan chan cmpp.DeliverResp
	commandChan     chan []byte
	deliverFakeChan chan []byte
	ATRespSeqID     chan uint32

	FlowVelocity *utils.FlowVelocity
	RateLimit    *utils.RateLimit

	SubmitToQueueCount  int64
	DeliverSendCount    int
	SeqID               uint32
	LastSeqID           uint32
	invalidMessageCount uint32
	activeTestCount     uint32
	submitTaskCount     int64
	deliverTaskCount    int64
	MsgID               uint64
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
		logger.Debug().Msgf("New connection：%v, Peer ip：%s", clientConn, sess.RemoteAddr)

		go HandleNewConn(clientConn, sess)
	}
}

func HandleNewConn(conn net.Conn, sess *Sessions) {
	var resp *cmpp.ConnResp
	var err error
	var runID string
	var data []byte

	qLen := utils.GetAttr("qlen")
	logger.Info().Msgf("队列缓冲值: %d", qLen)

	s := &SrvConn{
		conn:            conn,
		RemoteAddr:      sess.RemoteAddr,
		SubmitChan:      make(chan *cmpp.Submit, qLen),
		commandChan:     make(chan []byte, qLen),
		hsmChan:         make(chan HTTPSubmitMessageInfo, qLen),
		deliverRespChan: make(chan cmpp.DeliverResp, qLen),
		mapKeyInChan:    make(chan string, qLen),
		ExitSrv:         make(chan struct{}),
		ATRespSeqID:     make(chan uint32, 1),
		deliverMsgMap:   cmap.New[*deliverWithTime](),
		rw:              socket.NewPacketRW(conn),
		lsm: &LongSmsMap{
			LongSms: make(map[uint8]*LongSms),
			mLock:   new(sync.Mutex),
		},
		lsLock:  new(sync.Mutex),
		lsmLock: new(sync.Mutex),
		mutex:   new(sync.Mutex),
		BsiLock: new(sync.RWMutex),
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
	runID = s.Account.NickName + ":" + fmt.Sprintf("%p", s.conn)
	s.Logger = levellogger.NewLogger(runID)
	s.RunID = runID
	s.rw.RunId = runID

	s.waitGroup.Wrap(func() { s.ReadLoop() })
	time.Sleep(time.Duration(10) * time.Millisecond)
	s.waitGroup.Wrap(func() { SubmitMsgIDToQueue(s) })
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

	account := &AccountsInfo{}
	err = json.Unmarshal([]byte(str), account)
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
	s.Account = account
	s.Logger = levellogger.NewLogger(strconv.Itoa(int(s.Account.ID)))
	return resp, nil
}

func (s *SrvConn) CloseConnection() {
	if err := s.conn.Close(); err != nil {
		logger.Error().Msgf("通道(%s) conn Unexpected close error: %v", s.RunID, err)
	}
}

func (s *SrvConn) IsClosing() bool {
	return atomic.LoadInt32(&s.CloseFlag) == 1
}

func (s *SrvConn) Close() {
	runID := s.RunID
	if s.IsClosing() {
		logger.Debug().Msgf("帐号(%s) Connection has closed, Peer ip: %s", runID, s.RemoteAddr)
		return
	}
	atomic.StoreInt32(&(s.CloseFlag), 1)
	logger.Debug().Msgf("帐号(%s) conn will be closed: %v, Peer ip: %s", runID, s.conn, s.RemoteAddr)
	s.CloseConnection()
	logger.Debug().Msgf("帐号(%s) Connection closed, Peer ip: %s", runID, s.RemoteAddr)
}

func (s *SrvConn) Cleanup() {
	runID := s.RunID
	s.Logger.Debug().Msgf("帐号(%s) 关闭网络连接，并清理缓存中的数据", runID)

	for {
		t1 := len(s.mapKeyInChan)
		if t1 == 0 {
			break
		}
		select {
		case mapKey := <-s.mapKeyInChan: // 清理回执发送控制缓冲队列
			s.Logger.Debug().Msgf("帐号(%s) record Deliver mapKey：%s", runID, mapKey)
			if _, ok := s.deliverMsgMap.Get(mapKey); ok {
				s.deliverMsgMap.Remove(mapKey)
			}
		default:
		}

	}
	s.Logger.Debug().Msgf("帐号(%s) 缓存数据已清理，Exiting Cleanup...", runID)
}

func (s *SrvConn) ReadLoop() {
	atomic.StoreInt32(&s.ReadLoopRunning, 1)
	timer := time.NewTimer(utils.Timeout)
	runID := s.RunID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { s.HandleCommand(ctx) })
	// s.waitGroup.Wrap(func() { s.loopMakeMsgId(ctx) })
	s.waitGroup.Wrap(func() { s.LoopActiveTest(ctx) })

	for {
		data, err := s.rw.ReadPacket()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				time.Sleep(time.Duration(1) * time.Second)
				s.Logger.Error().Msgf("通道(%s) IO Timeout - %s",
					runID, err)
				continue
			}
			if err == io.EOF {
				s.Logger.Error().Msgf("通道(%s) IO EOF error %s, s.IsClosing:%v", runID, err, s.IsClosing())
				goto EXIT
			}
			if !strings.Contains(err.Error(), "use of closed network connection") {
				s.Logger.Error().Msgf("通道(%s) IO error - %s", runID, err)
			}
			goto EXIT
		}
		utils.ResetTimer(timer, utils.Timeout)

		select {
		case s.commandChan <- data:
			s.invalidMessageCount = 0
		case <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) 数据写入管道 s.commandChan 超时,s.commandChan len: %d",
				runID, len(s.commandChan))
			s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", runID, data)
		}
	}
EXIT:
	atomic.StoreInt32(&s.ReadLoopRunning, 0)
	s.waitGroup.Wrap(func() { s.Cleanup() })
	s.Logger.Debug().Msgf("账号(%s) Exiting ReadLoop...", runID)
}

func (s *SrvConn) HandleCommand(ctx context.Context) {
	runID := s.RunID
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	if s.Account.ConnFlowVelocity == 0 {
		s.Account.ConnFlowVelocity = 300
	}

	var rateLimit = utils.NewRateLimit(s.Account.ConnFlowVelocity, runID, runMode)
	s.RateLimit = rateLimit

	s.Logger.Debug().Msgf("账号(%s) 启动 HandleCommand 协程", runID)
	h := &cmpp.Header{}
	r := cmpp.NewActiveTestResp()

	for {
		select {
		case <-s.ExitSrv:
			s.Logger.Debug().Msgf("账号(%s) 接收到 s.ExitSrv 退出信号, 退出 HandleCommand 协程....", runID)
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
				// s.Logger.Debug().Msgf("账号(%s) 收到激活测试命令(CMPP_ACTIVE_TEST), SeqId: %d", runID, h.SeqId)
				r.SeqId = h.SeqId
				if err := r.IOWrite(s.rw); err != nil {
					s.Logger.Error().Msgf("发送数据失败,error:%v", err)
					s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runID)
					utils.CloseChan(&s.ExitSrv, s.mutex)
				}
				atomic.StoreUint32(&s.activeTestCount, 0)

			case common.CMPP_ACTIVE_TEST_RESP:
				// s.Logger.Debug().Msgf("账号(%s) 收到激活测试应答命令(CMPP_ACTIVE_TEST_RESP), SeqId: %d", runID, h.SeqId)
				select {
				case s.ATRespSeqID <- h.SeqId:
				case <-timer.C:
				}

			case common.CMPP_TERMINATE:
				s.Logger.Warn().Msgf("账号(%s) 收到拆除连接命令(CMPP_TERMINATE), SeqId:%d", runID, h.SeqId)
				s.Logger.Info().Msgf("账号(%s) 发送拆除连接应答命令(CMPP_TERMINATE_RESP)", runID)
				if err := cmpp.NewTerminateResp().IOWrite(s.rw); err != nil { //拆除连接
					s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE_RESP IOWrite: %v", runID, err)
				}
				goto EXIT

			case common.CMPP_TERMINATE_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到拆除连接应答命令(CMPP_TERMINATE_RESP), SeqId:%d", runID, h.SeqId)
				goto EXIT

			case common.CMPP_SUBMIT:
				count := atomic.AddInt64(&s.submitTaskCount, 1)
				if int(count) > 50 {
					s.Logger.Warn().Msgf("账号(%s) s.submitTaskCount: %d, sleep 100ms", runID, count)
					time.Sleep(time.Duration(100) * time.Millisecond)
				}
				s.waitGroup.Wrap(func() { s.handleSubmit(data) })

			case common.CMPP_DELIVER_RESP:
				atomic.AddInt64(&s.deliverTaskCount, 1)
				// s.waitGroup.Wrap(func() { s.handleDeliverResp(data) })

			default:
				s.Logger.Debug().Msgf("账号(%v) 命令未知, %v\n", runID, *h)
				// time.Sleep(time.Duration(1000) * time.Nanosecond)
			}
		case <-timer.C:
			//s.Logger.Debug().Msgf("账号(%s) HandleCommand Tick at", runID)
			select {
			case <-ctx.Done():
				s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号, 退出 HandleCommand 协程....", runID)
				goto EXIT
			default:
			}
		}
	}
EXIT:
	if !s.IsClosing() {
		s.Close() //关闭网络读写
	}
	s.Logger.Debug().Msgf("账号(%s) Exiting HandleCommand...", runID)
}

func (s *SrvConn) handleSubmit(data []byte) {
	var count uint64
	var err error
	h := &cmpp.Header{}
	h.UnPack(data[:common.CMPP_HEADER_SIZE])
	buf := data[common.CMPP_HEADER_SIZE:]
	runID := s.RunID
	resp := cmpp.NewSubmitResp()
	p := &cmpp.Submit{}
	resp.MsgId = <-utils.MsgIdChan
	if resp.MsgId == 0 {
		s.Logger.Error().Msgf("msgId generate error:")
		s.Logger.Error().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE)", runID)
		goto EXIT
	}
	resp.SeqId = h.SeqId
	count = atomic.AddUint64(&s.count, 1)

	resp.Result = p.UnPack(buf)
	if resp.Result == 0 {
		p.TotalLen = h.TotalLen
		p.SeqId = h.SeqId
		p.MsgId = resp.MsgId
		resp.Result = s.VerifySubmit(p)
	}

	if err = resp.IOWrite(s.rw); err != nil {
		s.Logger.Error().Msgf("账号(%s) SubmitResp IOWrite error: %v", runID, err)
		goto EXIT
	}

	if count%uint64(utils.PeekInterval) == 0 {
		s.Logger.Debug().Msgf("账号(%s) 收到消息提交(CMPP_SUBMIT), count: %d,resp.SeqId:%d,resp.MsgId:%d,"+
			"s.submitTaskCount:%d,s.SubmitChan len:%d", runID, s.count, resp.SeqId, resp.MsgId,
			atomic.LoadInt64(&s.submitTaskCount), len(s.SubmitChan))
	}

	s.LastSeqID = h.SeqId
	if resp.Result == 0 {
		if FakeGateway == 1 { //模拟网关服务器
			go s.makeDeliverMsg(p.MsgId, p.DestTerminalId)
		} else {
			timer := time.NewTimer(utils.Timeout * 10)
			select {
			case s.SubmitChan <- p:
			case t := <-timer.C:
				s.Logger.Debug().Msgf("账号(%s) 写入管道 s.SubmitChan 超时, Tick at: %v", runID, t)
				s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", s.RunID, p)
			}
		}
	} else {
		// 注意：需要断开处理
		s.Logger.Warn().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE),resp.Result:%d", runID, resp.Result)
		if err := cmpp.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
			s.Logger.Error().Msgf("账号(%s) Terminate IOWrite error: %v", runID, err)
		}
		time.Sleep(time.Duration(1) * time.Second)
		goto EXIT
	}
	atomic.AddInt64(&s.submitTaskCount, -1)
	return
EXIT:
	s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runID)
	utils.CloseChan(&s.ExitSrv, s.mutex)
	atomic.AddInt64(&s.submitTaskCount, -1)
}

func (s *SrvConn) VerifySubmit(p *cmpp.Submit) uint8 {
	runID := s.RunID
	if !s.RateLimit.Available() {
		s.Logger.Error().Msgf("账号(%s) 流速控制触发：%d", &runID, s.RateLimit.Lim.Burst())
		return common.ErrnoSubmitNotPassFlowControl
	}

	if s.LastSeqID == p.SeqId && s.LastSeqID != 0 {
		s.Logger.Error().Msgf("账号(%s) s.LastSeqId(%d) == h.SeqId(%d) && s.LastSeqId != 0",
			&runID, s.LastSeqID, p.SeqId)
		return common.ErrnoSubmitInvalidSequence
	}

	if p.RegisteredDelivery < 0 && p.RegisteredDelivery > 2 {
		s.Logger.Error().Msgf("账号(%s) 提交的信息:p.RegisteredDelivery != 0-2", runID)
		return common.ErrnoSubmitInvalidStruct
	}

	if p.MsgSrc.String() != s.Account.NickName {
		s.Logger.Error().Msgf("账号(%s) 提交的信息:s.Account.NickName != p.MsgSrc.String: %s", runID, p.MsgSrc.String())
		return common.ErrnoDeliverInvalidStruct
	}

	regexpStr := "^" + p.SrcId.String()
	re := regexp.MustCompile(s.Account.CmppDestID)
	if !re.MatchString(regexpStr) {
		s.Logger.Error().Msgf("账号(%s) 提交的信息:s.Account.CmppDestId !~ p.SrcId.String()", runID)
		return common.ErrnoSubmitInvalidSrcId
	}

	if len(p.SrcId.String()) < len(s.Account.CmppDestID) {
		s.Logger.Error().Msgf("账号(%s) 提交的信息:len(p.SrcId.String()) < len(s.Account.CmppDestId)", runID)
		return common.ErrnoSubmitInvalidSrcId
	}

	discard := true
	var status int64
	businessID := s.Account.BusinessID
	// s.Logger.Debug().Msgf("s.Account: %v,s.Account.BusinessInfo:%v", s.Account, s.Account.BusinessInfo)
	bsInfos := s.GetBusinessInfo()
	for _, v := range bsInfos {
		// s.Logger.Debug().Msgf("businessinfo: %v", v)
		if v.BusinessID == businessID {
			status = v.Status
			if status != 1 { // 服务不可用
				break
			}
			discard = false
		}
	}
	if discard {
		s.Logger.Error().Msgf("账号(%s) 提交的短信未找到对应的服务，businessId：%v, status:%v", runID, businessID, status)
		return common.ErrnoSubmitInvalidService
	}

	if p.TPUdhi == 1 { //长短信检验
		cLen := len(p.MsgContent)
		if cLen < 6 {
			s.Logger.Error().Msgf("账号(%s) 提交的长短信缺少头信息, cLen:%d", runID, cLen)
			return common.ErrnoSubmitInvalidStruct
		}
		byte4 := p.MsgContent[0:6][3]
		msgID := strconv.FormatUint(p.MsgId, 10)
		s.lsLock.Lock()
		defer s.lsLock.Unlock()
		if !s.lsm.exist(byte4) {
			ls := &LongSms{
				Content: make(map[uint8][]byte),
				MsgID:   make(map[uint8]string),
				mLock:   new(sync.Mutex),
			}
			s.lsm.set(byte4, ls)
		}
		ls := s.lsm.get(byte4)
		if ls.exist(p.PkNumber) {
			s.Logger.Error().Msgf("账号(%s) pkTotal:%d, pkNumber:%d, byte4:%d, msgID string:%s, msgID:%d 长短信标志位重复",
				s.RunID, p.PkTotal, p.PkNumber, byte4, msgID, p.MsgId)
			id, content := ls.get(p.PkNumber)
			s.Logger.Error().Msgf("old msgId:%s,content:%v", id, content)
			return common.ErrnoSubmitInvalidStruct
		}
		ls.set(p.PkNumber, msgID, p.MsgContent[6:])
		if utils.Debug {
			s.Logger.Debug().Msgf("账号(%s) pkTotal:%d, pkNumber:%d, byte4:%d, msgID string:%s, msgID:%d, s.longSms len:%d",
				s.RunID, p.PkTotal, p.PkNumber, byte4, msgID, p.MsgId, s.lsm.len())
		}
	} else if p.TPUdhi != 0 {
		s.Logger.Error().Msgf("账号(%s) 提交的TPPid值不合法:%d", runID, p.TPPid)
		return common.ErrnoSubmitInvalidStruct
	}
	return 0
}

func (s *SrvConn) handleDeliverResp(data []byte) {
	h := &cmpp.Header{}
	h.UnPack(data[:common.CMPP_HEADER_SIZE])
	buf := data[common.CMPP_HEADER_SIZE:]
	runID := s.RunID
	dr := &cmpp.DeliverResp{}
	dr.UnPack(buf)
	dr.SeqId = h.SeqId
	if dr.Result != 0 {
		s.Logger.Error().Msgf("账号(%s) 接收到的 CMPP_DELIVER_RESP Result: %d, msgId: %d",
			runID, dr.Result, dr.MsgId)
	}
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	select {
	case s.deliverRespChan <- *dr:
	case t := <-timer.C:
		s.Logger.Debug().Msgf("账号(%s) 写入管道 s.deliverRespChan 超时, Tick at: %v", runID, t)
	}
	atomic.AddInt64(&s.deliverTaskCount, -1)
}

func (s *SrvConn) UpdateBusinessInfo(newBsis []CmppBusinessInfo) {
	s.BsiLock.Lock()
	defer s.BsiLock.Unlock()
	s.Account.BusinessInfo = make([]CmppBusinessInfo, len(newBsis))
	copy(s.Account.BusinessInfo[:], newBsis)
}

func (s *SrvConn) UpdateAccout(account AccountsInfo) {
	s.Account.AccountHost = account.AccountHost
	s.Account.FreeTrial = account.FreeTrial
	s.Account.MarketFreeTrial = account.MarketFreeTrial
	s.Account.FlowVelocity = account.FlowVelocity
	s.Account.ConnFlowVelocity = account.ConnFlowVelocity

	newBsis := account.BusinessInfo
	if !slices.Equal(s.GetBusinessInfo(), newBsis) {
		s.Logger.Debug().Msgf("账号(%s) accout.BusinessInfo 已修改,newbBsis: %v", s.RunID, newBsis)
		s.UpdateBusinessInfo(newBsis)
	}
}

func (s *SrvConn) GetBusinessInfo() []CmppBusinessInfo {
	s.BsiLock.RLock()
	defer s.BsiLock.RUnlock()
	return s.Account.BusinessInfo
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

	runID := s.RunID
	hbTime := config.GetHBTime()
	timeout := hbTime.Timeout
	retry := hbTime.Retry
	tick := int(time.Duration(timeout) * time.Second / utils.Timeout)
	if tick == 0 {
		tick = 1
	}

	p := cmpp.NewActiveTest()
	p.SeqId = atomic.LoadUint32(&s.SeqID)
	s.Logger.Debug().Msgf("账号(%s) 启动 LoopActiveTest", runID)
	for {
		if atomic.LoadInt32(&s.ReadLoopRunning) == 0 {
			s.Logger.Debug().Msgf("通道(%s) c.ReadLoopRunning: 0， 退出LoopActiveTest", runID)
			goto EXIT
		}

		utils.ResetTimer(timer, utils.Timeout)
		select {
		case <-s.ATRespSeqID:
			//c.Logger.Debug().Msgf("账号(%s)接收到心跳应答包(CMPP_ACTIVE_TEST_RESP),RespSeqId: %d, timer1: %d, timer2: %d", chid, RespSeqId, timer1,timer2)
			sendTry = 0
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到ctx.Done信号，退出LoopActiveTest", runID)
			goto EXIT
		case <-timer.C:
		}

		if sendTry < retry && count%tick == 0 {
			p.SeqId = atomic.AddUint32(&s.SeqID, 1)
			//s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST), seqId: %d", runID, p.SeqId)
			if err := p.IOWrite(s.rw); err != nil {
				//s.Logger.Error().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST)失败:%v", runID, err)
				timer1 = tick + 1
				sendTry = 3
			}
			sendTry++
		}
		if timer1 > tick && sendTry >= retry {
			s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST) send try: %d and "+
				"接收心跳包命令(CMPP_ACTIVE_TEST_RESP) timeout: %d, will exit ", runID, sendTry, timer1)
			s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", runID)

			utils.CloseChan(&s.ExitSrv, s.mutex)
		}
		count++
		timer1 = int(atomic.AddUint32(&s.activeTestCount, 1))

	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting LoopActiveTest...", runID)
}
