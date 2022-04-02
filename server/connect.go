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
	"runtime"
	"sms_lib/config"
	"sms_lib/levellogger"
	"sms_lib/models"
	"sms_lib/protocol"
	"sms_lib/utils"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type SrvConn struct {
	conn       net.Conn
	addr       string
	RemoteAddr string
	Account    AccountsInfo

	RunId     string
	Logger    *levellogger.Logger
	rw        *protocol.PacketRW
	waitGroup utils.WaitGroupWrapper

	exitHandleCommandChan chan struct{}
	ReadLoopRunning       int32
	deliverSenderExit     int32
	CloseFlag             int32
	terminateSent         bool

	MsgId                 uint64
	SeqId                 uint32
	LastSeqId             uint32
	longSms               map[uint8]map[uint8][]byte
	longMsgId             map[uint8][]string
	deliverMsgMap         cmap.ConcurrentMap
	deliverResendCountMap cmap.ConcurrentMap

	hsmChan         chan HttpSubmitMessageInfo
	mapKeyInChan    chan string
	SubmitChan      chan protocol.Submit
	deliverRespChan chan protocol.DeliverResp
	commandChan     chan []byte
	deliverFakeChan chan []byte

	count               uint64
	SubmitToQueueCount  int
	DeliverSendCount    int
	FlowVelocity        *utils.FlowVelocity
	RateLimit           *utils.RateLimit
	invalidMessageCount uint32
	submitTaskCount     int64
	deliverTaskCount    int64
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
	var resp *protocol.ConnResp
	var err error
	var runId string
	var data []byte
	var strPoint string

	qLen := utils.GetAttr("qlen")
	logger.Info().Msgf("队列缓冲值: %d", qLen)

	s := &SrvConn{
		conn:                  conn,
		RemoteAddr:            sess.RemoteAddr,
		SubmitChan:            make(chan protocol.Submit, qLen),
		commandChan:           make(chan []byte, qLen),
		hsmChan:               make(chan HttpSubmitMessageInfo, qLen),
		deliverRespChan:       make(chan protocol.DeliverResp, qLen),
		mapKeyInChan:          make(chan string, qLen),
		exitHandleCommandChan: make(chan struct{}),
		deliverMsgMap:         cmap.New(),
		deliverResendCountMap: cmap.New(),
		rw:                    protocol.NewPacketRW(conn),
	}
	if FakeGateway == 1 {
		s.deliverFakeChan = make(chan []byte, qLen)
	}
	h := &protocol.Header{}

	s.rw.ReadTimeout = 10
	data, err = s.rw.ReadPacket()
	if err != nil {
		logger.Error().Msgf("s.rw.ReadPacket error:%v,read len:%d", err, len(data))
		goto EXIT
	}
	s.rw.Reset("r")

	h.UnPack(data[:12])
	if h.CmdId != protocol.CMPP_CONNECT {
		logger.Debug().Msgf("CmdId is not CMPP_CONNECT: %v", h)
		goto EXIT
	}

	resp, err = s.NewAuth(data[12:], sess)
	if err != nil {
		logger.Error().Msgf("s.NewAuth error: %v", err)
		goto EXIT
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

	strPoint = fmt.Sprintf("%p", s.conn)
	sess.Add(s.Account.NickName, strPoint)
	runId = s.Account.NickName + ":" + strPoint
	s.Logger = levellogger.NewLogger(runId)
	s.RunId = runId
	s.rw.RunId = runId
	InitChan(runId) //go通道缓冲初始化

	s.waitGroup.Wrap(func() { s.ReadLoop() })
	time.Sleep(time.Duration(10) * time.Millisecond)
	s.waitGroup.Wrap(func() { SubmitMsgIdToQueue(s) })
	s.waitGroup.Wrap(func() { DeliverPush(s) })
	s.waitGroup.Wrap(func() { s.LoopActiveTest() })

	s.waitGroup.Wait()
	sess.Done(s.Account.NickName, strPoint)
EXIT:
	s.Close()
	logger.Debug().Msgf("socket %v, Exiting HandleNewConn...", s.conn)
}

func (s *SrvConn) NewAuth(buf []byte, sess *Sessions) (*protocol.ConnResp, error) {
	var err error

	req := &protocol.ConnReq{}
	req.UnPack(buf)

	resp := protocol.NewConnResp()
	resp.SeqId = req.SeqId
	resp.TotalLen = protocol.HeaderLen + 1 + 16 + 1
	resp.Status = 0
	sourceAddr := req.SourceAddr
	user := sourceAddr.String()

	if len(buf) != 27 {
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectInvalidStruct]
		logger.Error().Msgf("len(buf) != 27 error:%v", err)
		resp.Status = protocol.ErrnoConnectInvalidStruct
		return nil, err
	}

	rKey := "index:user:userinfo:"
	str := models.RedisHGet(rKey, user)
	if str == "" {
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectAuthFaild]
		logger.Error().Msgf("账号(%s) 不存在, error:%v", user, err)
		resp.Status = protocol.ErrnoConnectAuthFaild
		return resp, err
	}

	var account AccountsInfo
	account = AccountsInfo{}
	err = json.Unmarshal([]byte(str), &account)
	if err != nil {
		logger.Error().Msgf("accounts json.unmarshal error:%v, exit...", err)
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectInvalidStruct]
		resp.Status = protocol.ErrnoConnectInvalidStruct
		return resp, err
	}
	if account.ConnFlowVelocity == 0 {
		account.ConnFlowVelocity = 100
	}

	currConns := sess.GetUserConns(user)
	maxConns := account.FlowVelocity / account.ConnFlowVelocity
	//isLogin, ok := sess.Users[user]
	//if ok && len(isLogin) == 5 {
	if currConns+1 > maxConns {
		logger.Error().Msgf("账号(%s) 当前建立的连接数已达最大允许连接数:%d", user, maxConns)
		resp.Status = protocol.ErrnoConnectAuthFaild
		return resp, err
	}

	//验证远程登录地址是否在白名单中
	whiteList := strings.Split(account.AccountHost, ",")
	if !collection.Collect(whiteList).Contains(sess.RemoteAddr) {
		logger.Error().Msgf("账号(%s) 登录ip地址(%s)非法!", sourceAddr.String(), sess.RemoteAddr)
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectInvalidSrcAddr]
		resp.Status = protocol.ErrnoConnectInvalidSrcAddr
		return resp, err
	}

	authSrc, err := protocol.GenAuthSrc(req.SourceAddr.String(), account.CmppPassword, req.Timestamp)
	if err != nil {
		logger.Error().Msgf("通道(%s) 生成authSrc信息错误：%v", sourceAddr.String(), err, authSrc)
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectAuthFaild]
		resp.Status = protocol.ErrnoConnectAuthFaild
		return resp, err
	}

	reqAuthSrc := req.AuthSrc.Byte()
	if !bytes.Equal(authSrc, reqAuthSrc) {
		logger.Error().Msgf("账号(%s) auth failed", sourceAddr.String())
		logger.Error().Msgf("authSrc: %v, req.AuthSrc: %v", authSrc, reqAuthSrc)
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectAuthFaild]
		resp.Status = protocol.ErrnoConnectAuthFaild
		return resp, err
	}

	authImsg, err := protocol.GenRespAuthSrc(resp.Status, string(authSrc), account.CmppPassword)
	if err != nil {
		logger.Debug().Msgf("通道(%s) 生成authImsg信息错误：%v", sourceAddr.String(), err, authImsg)
		err = protocol.ConnectRspResultErrMap[protocol.ErrnoConnectAuthFaild]
		resp.Status = protocol.ErrnoConnectAuthFaild
		return resp, err
	}

	resp.AuthIsmg = &protocol.OctetString{
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
	t := protocol.NewTerminate()
	timer := time.NewTimer(utils.Timeout)
	runId := s.RunId
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { s.HandleCommand(ctx) })
	s.waitGroup.Wrap(func() { s.loopMakeMsgId(ctx) })

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
		case <-utils.ExitSig.LoopRead[runId]:
			logger.Debug().Msgf("账号(%s) 收到utils.ExitSig.LoopRead信号，即将退出ReadLoop", runId)
			_ = t.IOWrite(s.rw)
			s.terminateSent = true
			time.Sleep(time.Duration(1) * time.Second)
			continue
		case <-utils.ExitSig.DeliverySender[runId]:
			logger.Debug().Msgf("账号(%s) 收到utils.ExitSig.DeliverySender信号，退出ReadLoop", runId)
			_ = t.IOWrite(s.rw)
			s.terminateSent = true
			time.Sleep(time.Duration(1) * time.Second)
			continue
		case <-s.exitHandleCommandChan: // 由HandleCommand close s.exitHandleCommandChan，
			logger.Debug().Msgf("账号(%s) 收到s.exitHandleCommandChan信号，退出ReadLoop", runId)
			goto EXIT
		case s.commandChan <- data:
			s.invalidMessageCount = 0
		case <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) 数据写入管道 s.commandChan 超时,s.commandChan len: %d",
				runId, len(s.commandChan))
			s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", runId, data)
		}
		if s.terminateSent {
			s.Logger.Debug().Msgf("账号(%s) 已发送过拆除连接命令，退出ReadLoop", runId)
			goto EXIT
		}
	}
EXIT:
	atomic.StoreInt32(&s.ReadLoopRunning, 0)
	if !s.IsClosing() {
		s.Close()
	}
	s.waitGroup.Wrap(func() { s.Cleanup() })
	s.Logger.Debug().Msgf("账号(%s) Exiting ReadLoop...", runId)
}

func (s *SrvConn) HandleCommand(ctx context.Context) {
	runId := s.RunId
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	//var unit = "ns"

	if s.Account.ConnFlowVelocity == 0 {
		s.Account.ConnFlowVelocity = 300
	}

	//var flowVelocity = &utils.FlowVelocity{
	//	CurrTime: utils.GetCurrTimestamp(unit),
	//	LastTime: utils.GetCurrTimestamp(unit),
	//	Rate:     s.Account.ConnFlowVelocity, //流速控制
	//	Unit:     unit,
	//	Duration: 1000000000, //纳秒
	//	RunMode:  runMode,
	//	RunId:    runId,
	//}
	//s.FlowVelocity = flowVelocity
	var rateLimit = utils.NewRateLimit(s.Account.ConnFlowVelocity, runId, runMode)
	s.RateLimit = rateLimit

	s.Logger.Debug().Msgf("账号(%s) 启动 HandleCommand 协程", runId)
	h := &protocol.Header{}
	for {
		utils.ResetTimer(timer, utils.Timeout)
		select {
		case data := <-s.commandChan:
			h.UnPack(data[:protocol.HeaderLen])
			switch h.CmdId {
			case protocol.CMPP_ACTIVE_TEST:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试命令(CMPP_ACTIVE_TEST), SeqId: %d", runId, h.SeqId)
				select {
				case utils.HbSeqId.SeqId[runId] <- h.SeqId:
				case <-timer.C:
				}

			case protocol.CMPP_ACTIVE_TEST_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试应答命令(CMPP_ACTIVE_TEST_RESP), SeqId: %d", runId, h.SeqId)
				select {
				case utils.HbSeqId.RespSeqId[runId] <- h.SeqId:
				case <-timer.C:
				}

			case protocol.CMPP_TERMINATE:
				s.Logger.Warn().Msgf("账号(%s) 收到拆除连接命令(CMPP_TERMINATE), SeqId:%d", runId, h.SeqId)
				s.Logger.Info().Msgf("账号(%s) 发送拆除连接应答命令(CMPP_TERMINATE_RESP)", runId)
				if err := protocol.NewTerminateResp().IOWrite(s.rw); err != nil { //拆除连接
					s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE_RESP IOWrite: %v", runId, err)
				}
				goto EXIT

			case protocol.CMPP_TERMINATE_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到拆除连接应答命令(CMPP_TERMINATE_RESP), SeqId", runId, h.SeqId)
				goto EXIT

			case protocol.CMPP_SUBMIT:
				count := atomic.AddInt64(&s.submitTaskCount, 1)
				if int(count) > 50 {
					s.Logger.Warn().Msgf("账号(%s) s.submitTaskCount: %d, sleep 100ms", runId, count)
					time.Sleep(time.Duration(100) * time.Millisecond)
				}
				s.waitGroup.Wrap(func() { s.handleSubmit(data) })

			case protocol.CMPP_DELIVER_RESP:
				atomic.AddInt64(&s.deliverTaskCount, 1)
				//if int(count) > config.GetQlen() {
				//	s.Logger.Warn().Msgf("通道(%s) 当前deliver任务数:%d",runId,count)
				//}
				s.waitGroup.Wrap(func() { s.handleDeliverResp(data) })

			default:
				s.Logger.Debug().Msgf("账号(%v) 命令未知, %v\n", runId, *h)
				// time.Sleep(time.Duration(1000) * time.Nanosecond)
			}
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号, 退出 HandleCommand 协程....", runId)
			goto EXIT
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
	if atomic.LoadInt32(&s.ReadLoopRunning) == 1 {
		close(s.exitHandleCommandChan)
	}
	if !s.IsClosing() {
		s.Close() //关闭网络读写
	}
	s.Logger.Debug().Msgf("账号(%s) Exiting HandleCommand...", runId)
}

func (s *SrvConn) handleDeliverResp(data []byte) {
	h := &protocol.Header{}
	h.UnPack(data[:protocol.HeaderLen])
	buf := data[protocol.HeaderLen:]
	runId := s.RunId
	dr := &protocol.DeliverResp{}
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

func (s *SrvConn) handleSubmit(data []byte) {
	var count uint64
	var err error
	h := &protocol.Header{}
	h.UnPack(data[:protocol.HeaderLen])
	buf := data[protocol.HeaderLen:]
	runId := s.RunId
	resp := protocol.NewSubmitResp()
	p := &protocol.Submit{}
	resp.MsgId = <-msgIdChan
	if resp.MsgId == 0 {
		s.Logger.Error().Msgf("msgId generate error:")
		s.Logger.Error().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE)", runId)
		if err := protocol.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
			s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE IOWrite: %v", runId, err)
		}
		time.Sleep(time.Duration(1) * time.Second)
		goto EXIT
	}
	resp.SeqId = h.SeqId
	count = atomic.AddUint64(&s.count, 1)
	//if count%uint64(s.FlowVelocity.Rate) == 0 {
	//	s.FlowVelocity.CurrTime = utils.GetCurrTimestamp(s.FlowVelocity.Unit)
	//	s.FlowVelocity.Control() //检测是否超速
	//}

	if !s.RateLimit.Available() {
		s.Logger.Error().Msgf("账号(%s) OverSpeed", runId)
		resp.Result = 8
	} else if len(buf)+12 != int(h.TotalLen) {
		s.Logger.Error().Msgf("账号(%s) buf len:%d + 12 != h.TotalLen:%d", runId, len(buf), h.TotalLen)
		resp.Result = protocol.ErrnoSubmitInvalidMsgLength
	} else if s.LastSeqId == h.SeqId && s.LastSeqId != 0 {
		s.Logger.Error().Msgf("账号(%s) s.LastSeqId(%d) == h.SeqId(%d) && s.LastSeqId != 0",
			runId, s.LastSeqId, h.SeqId)
		resp.Result = protocol.ErrnoSubmitInvalidSequence
	} else {
		if err := p.UnPack(buf); err != nil {
			s.Logger.Error().Msgf("账号(%s) p.UnPack(buf) error:%v", runId, err)
			resp.Result = protocol.ErrnoSubmitInvalidStruct
		}
	}

	if resp.Result == 0 {
		if p.TPPid > 1 {
			s.Logger.Error().Msgf("账号(%s)  p.TPPid > 1", runId)
			resp.Result = 1
		}
	}
	err = resp.IOWrite(s.rw)
	if err != nil {
		s.Logger.Error().Msgf("账号(%s) SubmitResp IOWrite error: %v", runId, err)
		goto EXIT
	}

	if count%uint64(utils.PeekInterval) == 0 {
		s.Logger.Debug().Msgf("账号(%s) 收到消息提交(CMPP_SUBMIT), count: %d,resp.SeqId:%d,resp.MsgId:%d,"+
			"s.submitTaskCount:%d", runId, s.count, resp.SeqId, resp.MsgId, atomic.LoadInt64(&s.submitTaskCount))
	}

	s.LastSeqId = h.SeqId
	if resp.Result == 0 {
		p.SeqId = h.SeqId
		p.MsgId = resp.MsgId

		if FakeGateway == 1 { //模拟网关服务器
			go s.makeDeliverMsg(p.MsgId, p.DestTerminalId)
		} else {
			timer := time.NewTimer(utils.Timeout * 10)
			select {
			case s.SubmitChan <- *p:
			case t := <-timer.C:
				s.Logger.Debug().Msgf("账号(%s) 写入管道 s.SubmitChan 超时, Tick at: %v", runId, t)
				s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", s.RunId, p)
			}
		}
	} else {
		if resp.Result == protocol.ErrnoSubmitInvalidMsgLength ||
			resp.Result == protocol.ErrnoSubmitInvalidSequence ||
			resp.Result == protocol.ErrnoSubmitInvalidStruct {
			s.Logger.Warn().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE),resp.Result:%d", runId, resp.Result)
			if err := protocol.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
				s.Logger.Error().Msgf("账号(%s) Terminate IOWrite error: %v", runId, err)
			}
			time.Sleep(time.Duration(1) * time.Second)
		}
	}
	atomic.AddInt64(&s.submitTaskCount, -1)
	return
EXIT:
	if !utils.ChIsClosed(s.exitHandleCommandChan) {
		close(s.exitHandleCommandChan)
	}
	atomic.AddInt64(&s.submitTaskCount, -1)
	s.Logger.Error().Msgf("账号(%s) close(s.exitHandleCommandChan), will exit HandleCommand", runId)
}

func (s *SrvConn) LoopActiveTest() {
	//通信双方以客户-服务器方式建立 TCP 连接，用于双方信息的相互提交。当信道上没有数据
	//传输时，通信双方应每隔时间 C 发送链路检测包以维持此连接，当链路检测包发出超过时
	//间 T 后未收到响应，应立即再发送链路检测包，再连续发送 N-1 次后仍未得到响应则断开
	//此连接
	// c=60s, t=10, n=3
	var length = protocol.HeaderLen // header length
	var timer1 = 0                  // 对端发送CMPP_ACTIVE_TEST超时计时
	var timer2 = 0                  // 对端发送CMPP_ACTIVE_TEST_RESP超时计时
	var sendTry = 0                 //发送CMPP_ACTIVE_TEST到对端尝试次数，max=3

	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()

	//chid := int(s.Account.Id)
	runId := s.RunId
	hbTime := config.GetHBTime()
	timeout := hbTime.Timeout
	delay := hbTime.Delay
	retry := hbTime.Retry

	r := &protocol.ActiveTestResp{}
	r.Header = &protocol.Header{}
	r.TotalLen = length + 1
	r.CmdId = protocol.CMPP_ACTIVE_TEST_RESP

	p := &protocol.ActiveTest{}
	p.Header = &protocol.Header{}
	p.TotalLen = length
	p.CmdId = protocol.CMPP_ACTIVE_TEST
	p.SeqId = atomic.LoadUint32(&s.SeqId)
	RespSeqId := p.SeqId
	s.Logger.Debug().Msgf("账号(%s) 启动心跳协程 LoopActiveTest", runId)
	for {
		if atomic.LoadInt32(&s.ReadLoopRunning) == 0 {
			goto EXIT
		}
		utils.ResetTimer(timer, utils.Timeout)
		select {
		case recvSeqId := <-utils.HbSeqId.SeqId[runId]:
			r.SeqId = recvSeqId
			//logger.Debug().Msgf("账号(%s) 接收到心跳包(CMPP_ACTIVE_TEST), recvSeqId: %d, timer1: %d, timer2: %d",
			//	runId, recvSeqId, timer1, timer2)
			err := r.IOWrite(s.rw)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) 发送心跳应答包命令(CMPP_ACTIVE_TEST_RESP) error: %v", runId, err)
				//if !strings.Contains(err.Error(), "connection reset by peer") {
				//	s.Logger.Error().Msgf("通道(%s) IO error - %s", runId, err)
				//}
				timer1 = 120
				sendTry = 3
			} else {
				timer1 = 0
			}

		case RespSeqId = <-utils.HbSeqId.RespSeqId[runId]:
			//c.Logger.Debug().Msgf("账号(%s)接收到心跳应答包(CMPP_ACTIVE_TEST_RESP),RespSeqId: %d, timer1: %d, timer2: %d", chid, RespSeqId, timer1,timer2)
			sendTry = 0

		//case <-utils.NotifySig.IsTransfer[runId]: //通信信道上有数据不发送检测包
		//	timer2 = 1
		//	sendTry = 0
		case <-s.exitHandleCommandChan:
			s.Logger.Debug().Msgf("账号(%s) 接收到c.exitHandleCommandChan信号，退出LoopActiveTest", runId)
			goto EXIT
		case <-timer.C:
		}

		if p.SeqId > RespSeqId && timer2%delay == 0 && sendTry < retry {
			s.Logger.Debug().Msgf("账号(%s) 接收激活测试应答命令(CMPP_ACTIVE_TEST_RESP)失败, SeqId:%d, "+
				"RespSeqId:%d,send_try:%d", runId, p.SeqId, RespSeqId, sendTry)
			sendTry++
			timer2 = 0
		}
		//c.Logger.Debug().Msgf("timer1:%d,timer2:%d,sendTry:%d",timer1,timer2,sendTry)
		if sendTry < retry && timer2%timeout == 0 {
			p.SeqId = atomic.AddUint32(&s.SeqId, 1)
			s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST), seqId: %d", runId, p.SeqId)
			err := p.IOWrite(s.rw)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST)失败:%v", runId, err)
				timer1 = 120
				sendTry = 3
			}
			timer2 = 0
		}
		if timer1 >= 60 && sendTry >= retry {
			utils.ExitSig.LoopActiveTest[runId] <- true
			s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST) send try: %d and "+
				"接收心跳包命令(CMPP_ACTIVE_TEST_RESP) timeout: %d, will exit ", runId, sendTry, timer1)
			time.Sleep(time.Duration(5) * time.Second)
			goto EXIT
		}
		timer1++
		timer2++
	}
EXIT:
	s.Close()
	s.Logger.Debug().Msgf("账号(%s) Exiting LoopActiveTest...", runId)
}
