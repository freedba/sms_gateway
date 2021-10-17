package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

var mLock = sync.Mutex{}

type SrvConn struct {
	conn       net.Conn
	addr       string
	RemoteAddr string
	Account    AcountsInfo

	RunId  string
	Logger *levellogger.Logger
	//Wg                    *sync.WaitGroup
	exitHandleCommandChan chan struct{}
	ReadLoopRunning       int32
	deliverSenderExit     int32
	CloseFlag             int32
	exitSignalChan        chan struct{}

	MsgId         uint64
	SeqId         uint32
	LastSeqId     uint32
	longSms       map[uint8]map[uint8][]byte
	deliverMsgMap cmap.ConcurrentMap

	hsmChan         chan HttpSubmitMessageInfo
	mapKeyInChan    chan string
	SubmitChan      chan protocol.Submit
	deliverRespChan chan protocol.DeliverResp
	commandChan     chan []byte

	count               uint64
	SubmitToQueueCount  int
	DeliverSendCount    int
	FlowVelocity        *utils.FlowVelocity
	invalidMessageCount uint32

	rw        *protocol.PacketRW
	waitGroup utils.WaitGroupWrapper
}

type Sessions struct {
	ln         net.Listener
	Users      map[string]int
	Conns      map[*SrvConn]struct{}
	RemoteAddr string
}

func Listen() {

	addr := config.GetListen()
	if addr == "" {
		addr = "0.0.0.0:7890"
	}
	logger.Debug().Msgf("start Server at %s", addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error().Msgf("error listening: ", err.Error())
		return
	}
	defer func(ln net.Listener) {
		err := ln.Close()
		if err != nil {
			logger.Error().Msgf("ln.Close error: %v", err)
		}
	}(ln)

	sess := &Sessions{
		Users: make(map[string]int),
		ln:    ln,
		Conns: make(map[*SrvConn]struct{}),
	}
	go signalHandle(sess)

	for {
		clientConn, err := ln.Accept()
		if err != nil {
			logger.Error().Msgf("error Listen Accept: %v", err.Error())
			if nerr, ok := err.(net.Error); ok && nerr.Temporary() {
				logger.Error().Msgf("NOTICE: temporary Accept() failure - %s", err)
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

	qLen := config.GetQlen()
	if qLen < 16 {
		qLen = 16
	}
	s := &SrvConn{
		conn:                  conn,
		RemoteAddr:            sess.RemoteAddr,
		SubmitChan:            make(chan protocol.Submit, qLen),
		commandChan:           make(chan []byte, qLen),
		hsmChan:               make(chan HttpSubmitMessageInfo, qLen),
		deliverRespChan:       make(chan protocol.DeliverResp, qLen),
		mapKeyInChan:          make(chan string, qLen),
		exitHandleCommandChan: make(chan struct{}),
		exitSignalChan:        make(chan struct{}),
		deliverMsgMap:         cmap.New(),
		rw:                    protocol.NewPacketRW(conn),
	}

	var buf []byte
	h := &protocol.Header{}
	s.rw.ReadTimeout = 10
	data, err := s.rw.ReadPacket()
	if err != nil {
		logger.Error().Msgf("s.rw.ReadPacket error:%v,read len:%d", err)
		goto EXIT
	}
	s.rw.Reset()

	h.UnPack(data[:12])
	if h.CmdId != protocol.CMPP_CONNECT {
		logger.Debug().Msgf("CmdId is not CMPP_CONNECT: %v", h)
		goto EXIT
	}

	buf = data[12:]
	if len(buf) != 27 {
		err = errors.New(fmt.Sprintf("invalid message length, buf len:%d", len(buf)))
		logger.Error().Msgf("len(buf) != 27 error:%d", err)
		goto EXIT
	}

	resp, err = s.NewAuth(buf, sess)
	if err != nil {
		logger.Error().Msgf("s.NewAuth error: %v", err)
		goto EXIT
	}

	if err = resp.IOWrite(s.rw); err != nil {
		logger.Error().Msgf("resp.IOWrite error: %v", err)
		goto EXIT
	}

	if resp.Status != 0 {
		time.Sleep(time.Duration(1) * time.Second)
		goto EXIT
	}

	mLock.Lock()
	sess.Users[s.Account.NickName]++
	sess.Conns[s] = struct{}{}
	mLock.Unlock()
	runId = s.Account.NickName + ":" + strconv.Itoa(sess.Users[s.Account.NickName])
	s.Logger = levellogger.NewLogger(runId)
	s.RunId = runId
	s.rw.RunId = runId
	InitChan(runId) //go通道缓冲初始化
	s.waitGroup.Wrap(func() { s.ReadLoop() })
	time.Sleep(time.Duration(10) * time.Millisecond)
	s.waitGroup.Wrap(func() { SubmitMsgIdToQueue(s) })
	s.waitGroup.Wrap(func() { DeliverSend(s) })
	s.waitGroup.Wrap(func() { s.LoopActiveTest() })
	s.waitGroup.Wait()

EXIT:
	s.Close()
	mLock.Lock()
	if sess.Users[s.Account.NickName] > 0 {
		sess.Users[s.Account.NickName]--
	}
	if sess.Users[s.Account.NickName] == 0 {
		delete(sess.Users, s.Account.NickName)
	}
	delete(sess.Conns, s)
	mLock.Unlock()
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

	isLogin, ok := sess.Users[sourceAddr.String()]
	if ok && isLogin > 0 {
		logger.Error().Msgf("账号(%s) 已登录过", sourceAddr.String())
		//return utils.Errs[utils.ErrNoConnAuthFailed]
	}

	rKey := "index:user:userinfo:"
	str := models.RedisHGet(rKey, sourceAddr.String())
	if str == "" {
		logger.Error().Msgf("账号(%s) 不存在", sourceAddr.String())
		err = utils.Errs[utils.ErrNoConnInvalidStruct]
		resp.Status = utils.ErrNoConnAuthFailed
		return resp, err
	}

	var account AcountsInfo
	account = AcountsInfo{}
	err = json.Unmarshal([]byte(str), &account)
	if err != nil {
		logger.Error().Msgf("accounts json.unmarshal error:%v, exit...", err)
		resp.Status = utils.ErrNoConnInvalidStruct
		err = utils.Errs[utils.ErrNoConnInvalidStruct]
		return resp, err
	}

	//验证远程登录地址是否在白名单中
	whiteList := strings.Split(account.AccountHost, ",")
	if !collection.Collect(whiteList).Contains(sess.RemoteAddr) {
		logger.Error().Msgf("账号(%s) 登录ip地址(%s)非法!", sourceAddr.String(), sess.RemoteAddr)
		resp.Status = utils.ErrNoConnInvalidSrcAddr
		err = utils.Errs[utils.ErrNoConnInvalidSrcAddr]
		return resp, err
	}

	authSrc, err := protocol.GenAuthSrc(req.SourceAddr.String(), account.CmppPassword, req.Timestamp)
	if err != nil {
		logger.Error().Msgf("通道(%s) 生成authSrc信息错误：%v", sourceAddr.String(), err, authSrc)
		resp.Status = utils.ErrNoConnInvalidSrcAddr
		err = utils.Errs[utils.ErrNoConnInvalidSrcAddr]
		return resp, err
	}

	reqAuthSrc := req.AuthSrc.Byte()
	if !bytes.Equal(authSrc, reqAuthSrc) {
		logger.Error().Msgf("账号(%s) auth failed", sourceAddr.String())
		logger.Error().Msgf("authsrc: %v, req.Authsrc: %v", authSrc, reqAuthSrc)
		err = utils.Errs[utils.ErrNoConnAuthFailed]
		resp.Status = utils.ErrNoConnAuthFailed
		return resp, err
	}

	authImsg, err := protocol.GenRespAuthSrc(resp.Status, string(authSrc), account.CmppPassword)
	if err != nil {
		logger.Debug().Msgf("通道(%s) 生成authImsg信息错误：%v", sourceAddr.String(), err, authImsg)
		resp.Status = utils.ErrNoConnAuthFailed
		err = utils.Errs[utils.ErrNoConnAuthFailed]
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
		//logger.Debug().Msgf("t1: %d, t2: %d, t3:%d",t1,t2,t3)
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
	s.Logger.Debug().Msgf("帐号(%s) 完成缓存数据清理，Exiting Cleanup...", runId)
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
		case msgIdChan <- generateMsgID():
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
	//s.Wg.Add(2)
	s.waitGroup.Wrap(func() { s.HandleCommand(ctx) })
	s.waitGroup.Wrap(func() { s.loopMakeMsgId(ctx) })
	//go s.HandleCommand(ctx)
	//go s.loopMakeMsgId(ctx)

	for {
		data, err := s.rw.ReadPacket()
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				time.Sleep(time.Duration(1) * time.Second)
				s.Logger.Error().Msgf("通道(%s) IO Timeout - %s",
					runId, err)
				continue
			}
			if err == io.EOF && s.IsClosing() {
				s.Logger.Error().Msgf("通道(%s) IO error - %s, s.IsClosing:%v", runId, err, s.IsClosing())
				goto EXIT
			}
			if !strings.Contains(err.Error(), "use of closed network connection") {
				s.Logger.Error().Msgf("通道(%s) IO error - %s", runId, err)
			}
			goto EXIT
			//s.Logger.Error().Msgf("账号(%s) conn read error: %v", runId, err)
			//if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			//	if atomic.LoadInt32(&s.deliverSenderExit) == 1 {
			//		s.Logger.Debug().Msgf("账号(%s) deliverSender 协程已退出", runId)
			//		goto EXIT
			//	}
			//	//s.Logger.Debug().Msgf("账号(%s) timeout: %v", chid, err)
			//	continue
			//} else if err.Error() == "invalid message length" {
			//	s.invalidMessageCount++
			//	if s.invalidMessageCount < 10 {
			//		s.Logger.Error().Msgf("账号(%s) ReadLoop error: %v, s.invalidMessageCount:%d",
			//			runId, err, s.invalidMessageCount)
			//		continue
			//	}
			//	s.Logger.Warn().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE)", runId)
			//	if err := protocol.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
			//		s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE IOWrite: %v", runId, err)
			//	}
			//	time.Sleep(time.Duration(1) * time.Second)
			//	s.Logger.Error().Msgf("账号(%s) will exit LoopRead", runId)
			//	goto EXIT
			//} else {
			//	s.Logger.Error().Msgf("账号(%s) s.rw.ReadPacket error: %v", runId, err)
			//	goto EXIT
			//}
		}
		utils.ResetTimer(timer, utils.Timeout)
		select {
		case <-s.exitSignalChan:
			logger.Debug().Msgf("账号(%s) 收到s.exitSignalChan信号，退出ReadLoop", runId)
			_ = t.IOWrite(s.rw)
			goto EXIT
		case <-utils.ExitSig.DeliverySender[s.RunId]:
			logger.Debug().Msgf("账号(%s) 收到utils.ExitSig.DeliverySender信号，退出ReadLoop", runId)
			_ = t.IOWrite(s.rw)
			goto EXIT
		case <-s.exitHandleCommandChan: // 由HandleCommand close s.exitHandleCommandChan，
			logger.Debug().Msgf("账号(%s) 收到s.exitHandleCommandChan信号，退出ReadLoop", runId)
			goto EXIT
		case s.commandChan <- data:
			s.invalidMessageCount = 0
		case <-timer.C:
			logger.Debug().Msgf("账号(%s) 数据写入管道 s.commandChan 超时,s.commandChan len: %d",
				runId, len(s.commandChan))
			logger.Debug().Msgf("账号(%s) record Submit: %v ", runId, data)
		}
	}
EXIT:
	atomic.StoreInt32(&s.ReadLoopRunning, 0)
	//s.Wg.Add(1)
	s.waitGroup.Wrap(func() { s.Cleanup() })
	//go s.Cleanup()
	s.Logger.Debug().Msgf("账号(%s) Exiting ReadLoop...", runId)
}

func (s *SrvConn) HandleCommand(ctx context.Context) {
	runId := s.RunId
	timer := time.NewTimer(utils.Timeout)
	var unit = "ns"
	var flowVelocity = &utils.FlowVelocity{
		CurrTime: utils.GetCurrTimestamp(unit),
		LastTime: utils.GetCurrTimestamp(unit),
		Rate:     s.Account.FlowVelocity, //流速控制
		Unit:     unit,
		Duration: 1000000000, //纳秒
		RunMode:  runmode,
		RunId:    runId,
	}
	s.FlowVelocity = flowVelocity
	t := protocol.NewTerminate()
	s.Logger.Debug().Msgf("账号(%s) 启动 HandleCommand 协程", runId)
	h := &protocol.Header{}
	for {
		utils.ResetTimer(timer, utils.Timeout)
		select {
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 HandleCommand 协程....", runId)
			goto EXIT
		case data := <-s.commandChan:
			//buf := data[:12]
			h.UnPack(data[:12])
			//buf := data[12:]
			//s.Logger.Debug().Msgf("buf:%v,buf len:%d, header:%v", buf, len(buf),*h)
			switch h.CmdId {
			case protocol.CMPP_ACTIVE_TEST:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试命令(CMPP_ACTIVE_TEST), SeqId: %d", runId, h.SeqId)
				utils.HbSeqId.SeqId[runId] <- h.SeqId

			case protocol.CMPP_ACTIVE_TEST_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到激活测试应答命令(CMPP_ACTIVE_TEST_RESP), SeqId: %d", runId, h.SeqId)
				utils.HbSeqId.RespSeqId[runId] <- h.SeqId

			case protocol.CMPP_TERMINATE:
				s.Logger.Warn().Msgf("账号(%s) 收到拆除连接命令(CMPP_TERMINATE), SeqId:%d", runId, h.SeqId)
				s.Logger.Info().Msgf("账号(%s) 发送拆除连接应答命令(CMPP_TERMINATE_RESP)", runId)
				if err := protocol.NewTerminateResp().IOWrite(s.rw); err != nil { //拆除连接
					s.Logger.Error().Msgf("通道(%s) CMPP_TERMINATE_RESP IOWrite: %v", runId, err)
				}
				time.Sleep(time.Duration(1) * time.Second)
				goto EXIT

			case protocol.CMPP_TERMINATE_RESP:
				s.Logger.Debug().Msgf("账号(%s) 收到拆除连接应答命令(CMPP_TERMINATE_RESP), SeqId", runId, h.SeqId)
				goto EXIT

			case protocol.CMPP_SUBMIT:

				//p := &protocol.Submit{}
				//
				//if err := p.UnPack(buf); err != nil {
				//	s.Logger.Error().Msgf("账号(%s),data:%v,buf len:%d,seqId:%d", runId, data,len(data),h.SeqId)
				//}
				//s.Logger.Debug().Msgf("账号(%s),-----",runId)
				//s.Wg.Add(1)
				s.waitGroup.Wrap(func() { s.handleSubmit(data) })
				//go s.handleSubmit(*h, buf)

			case protocol.CMPP_DELIVER_RESP:
				//s.Wg.Add(1)
				s.waitGroup.Wrap(func() { s.handleDeliverResp(data) })
				//go s.handleDeliverResp(*h, buf)

			default:
				s.Logger.Debug().Msgf("账号(%v) 命令未知, %v\n", runId, *h)
				// time.Sleep(time.Duration(1000) * time.Nanosecond)
			}
		case <-s.exitSignalChan:
			logger.Debug().Msgf("账号(%s) 收到s.exitSignalChan信号，退出ReadLoop", runId)
			_ = t.IOWrite(s.rw)
			goto EXIT
		case t := <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) HandleCommand Tick at: %v", runId, t)
		}
	}
EXIT:
	if atomic.LoadInt32(&s.ReadLoopRunning) == 1 {
		close(s.exitHandleCommandChan)
	}
	s.Close() //关闭网络读写
	s.Logger.Debug().Msgf("账号(%s) Exiting HandleCommand...", runId)
}

func (s *SrvConn) handleDeliverResp(data []byte) {
	h := &protocol.Header{}
	h.UnPack(data[:12])
	buf := data[12:]
	runId := s.RunId
	dr := &protocol.DeliverResp{}
	dr.UnPack(buf)
	dr.SeqId = h.SeqId
	if dr.Result != 0 {
		s.Logger.Error().Msgf("账号(%s) 接收到的 CMPP_DELIVER_RESP Result: %d, msgid: %d",
			runId, dr.Result, dr.MsgId)
	}
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	select {
	case s.deliverRespChan <- *dr:
	case t := <-timer.C:
		s.Logger.Debug().Msgf("账号(%s) d写入管道 s.deliverRespChan 超时, Tick at: %v", runId, t)
	}
}

func (s *SrvConn) handleSubmit(data []byte) {
	h := &protocol.Header{}
	h.UnPack(data[:12])
	buf := data[12:]
	//logger.Debug().Msgf("header:%v, buf len:%d",h, len(buf))
	var count uint64
	var err error
	runId := s.RunId
	resp := protocol.NewSubmitResp()
	p := &protocol.Submit{}
	resp.MsgId = <-msgIdChan
	if resp.MsgId == 0 {
		s.Logger.Error().Msgf("msgid generate error:")
		s.Logger.Error().Msgf("账号(%s) 发送拆除连接命令(CMPP_TERMINATE)", runId)
		if err := protocol.NewTerminate().IOWrite(s.rw); err != nil { //拆除连接
			s.Logger.Error().Msgf("账号(%s) CMPP_TERMINATE IOWrite: %v", runId, err)
		}
		time.Sleep(time.Duration(1) * time.Second)
		goto EXIT
	}
	resp.SeqId = h.SeqId
	count = atomic.AddUint64(&s.count, 1)
	if count%uint64(s.FlowVelocity.Rate) == 0 {
		s.FlowVelocity.CurrTime = utils.GetCurrTimestamp(s.FlowVelocity.Unit)
		s.FlowVelocity.Control() //检测是否超速
	}
	if s.FlowVelocity.OverSpeed {
		s.Logger.Error().Msgf("账号(%s) s.FlowVelocity.OverSpeed:%v", runId, s.FlowVelocity.OverSpeed)
		resp.Result = 8
		//s.FlowVelocity.OverSpeed = false
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
		s.Logger.Debug().Msgf("账号(%s) 收到消息提交(CMPP_SUBMIT), count: %d,resp.SeqId:%d,"+
			"resp.MsgId:%d", runId, s.count, resp.SeqId, resp.MsgId)
	}

	s.LastSeqId = h.SeqId
	if resp.Result == 0 {
		p.SeqId = h.SeqId
		p.MsgId = resp.MsgId
		timer := time.NewTimer(utils.Timeout)
		select {
		case s.SubmitChan <- *p: //理论上不会阻塞，通道数据首先会入nsq,失败后会入mysql
		case t := <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) 写入管道 s.SubmitChan 超时, Tick at: %v", runId, t)
			s.Logger.Debug().Msgf("账号(%s) record Submit: %v ", s.RunId, p)
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
	return
EXIT:
	if !utils.ChIsClosed(s.exitHandleCommandChan) {
		close(s.exitHandleCommandChan)
	}
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
		select {
		case recvSeqId := <-utils.HbSeqId.SeqId[runId]:
			r.SeqId = recvSeqId
			logger.Debug().Msgf("账号(%s) 接收到心跳包(CMPP_ACTIVE_TEST), recvSeqId: %d, timer1: %d, timer2: %d",
				runId, recvSeqId, timer1, timer2)
			err := r.IOWrite(s.rw)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) 发送心跳应答包命令(CMPP_ACTIVE_TEST_RESP) error: %v", runId, err)
			}
			timer1 = 0

		case RespSeqId = <-utils.HbSeqId.RespSeqId[runId]:
			//c.Logger.Debug().Msgf("账号(%s)接收到心跳应答包(CMPP_ACTIVE_TEST_RESP),RespSeqId: %d, timer1: %d, timer2: %d", chid, RespSeqId, timer1,timer2)
			sendTry = 0

		//case <-utils.NotifySig.IsTransfer[runId]: //通信信道上有数据不发送检测包
		//	timer2 = 1
		//	sendTry = 0
		case <-s.exitHandleCommandChan:
			s.Logger.Debug().Msgf("账号(%s) 接收到c.exitHandleCommandChan信号，退出LoopActiveTest", runId)
			goto EXIT
		default:
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
		if timer1 > 120 && sendTry >= retry {
			utils.ExitSig.LoopActiveTest[runId] <- true
			s.Logger.Debug().Msgf("账号(%s) 发送心跳包命令(CMPP_ACTIVE_TEST) send try: %d and "+
				"接收心跳包命令(CMPP_ACTIVE_TEST_RESP) timeout: %d, will exit ", runId, sendTry, timer1)
			time.Sleep(time.Duration(5) * time.Second)
			goto EXIT
		}
		timer1++
		timer2++
		time.Sleep(time.Duration(2) * time.Second)
	}
EXIT:
	//if atomic.LoadInt32(&s.LoopSendRunning) == 1 {
	//	utils.ExitSig.LoopSend[chid] <- true
	//}
	s.Logger.Debug().Msgf("账号(%s) Exiting LoopActiveTest...", runId)
}
