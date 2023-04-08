package server

import (
	"context"
	"encoding/json"
	"errors"
	"sms_lib/config"
	"sms_lib/models"
	"sms_lib/protocol/cmpp"
	"sms_lib/protocol/common"
	"sms_lib/utils"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/youzan/go-nsq"
)

type deliverSender struct {
	deliverNmc *models.MessageHandler
	moNmc      *models.MessageHandler
	wg         *sync.WaitGroup
	s          *SrvConn
	stopCh     chan struct{}
	mutex      *sync.Mutex
}

func DeliverPush(s *SrvConn) {
	cfg := config.GetTopicPrefix()
	uid := s.Account.ID
	// user := s.Account.NickName
	var topicPrefix string
	var moNmc *models.MessageHandler
	var wg sync.WaitGroup
	var mutex = sync.Mutex{}
	var snd *deliverSender
	stopCh := make(chan struct{})

	topicPrefix = cfg.DeliverSend
	// topicName := topicPrefix + strconv.FormatInt(uid, 10)
	deliverNmc := models.InitConsumer(models.WithTopic(topicPrefix), models.WithRunID(strconv.Itoa(int(uid))))

	topicPrefix = cfg.DeliverMoSend
	// topicName = topicPrefix + strconv.FormatInt(uid, 10)
	moNmc = models.InitConsumer(models.WithTopic(topicPrefix), models.WithRunID(strconv.Itoa(int(uid))))

	snd = &deliverSender{
		wg:         &wg,
		stopCh:     stopCh,
		s:          s,
		mutex:      &mutex,
		deliverNmc: deliverNmc,
		moNmc:      moNmc,
	}

	snd.consumeDeliverMsg()
	time.Sleep(time.Duration(1) * time.Second)
	snd.cleanChan(deliverNmc.MsgChan)
	snd.cleanChan(moNmc.MsgChan)
	// EXIT:
	atomic.StoreInt32(&s.deliverSenderExit, 1)
	if atomic.LoadInt32(&s.ReadLoopRunning) == 1 {
		s.Logger.Debug().Msgf("账号(%s) close(c.ExitSrv)")
		utils.CloseChan(&s.ExitSrv, s.mutex)
	}
	s.Logger.Debug().Msgf("账号(%s) Exiting DeliverSend...", s.RunID)
}

func (snd *deliverSender) consumeDeliverMsg() {
	s := snd.s
	var err error
	var exitFlag = false
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	deliverNmc := snd.deliverNmc
	moNmc := snd.moNmc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { snd.handleDeliverResp(ctx) })

	for {
		utils.ResetTimer(timer, utils.Timeout)
		if s.IsClosing() || s.ReadLoopRunning == 0 {
			s.Logger.Debug().Msgf("账号(%s) s.IsClosing:%v,s.ReadLoopRunning:%d",
				s.RunID, s.IsClosing(), s.ReadLoopRunning)
			exitFlag = true
		}
		//logger.Debug().Msgf("deliverNmc.MsgChan:%d,moNmc.MsgChan:%d",len(deliverNmc.MsgChan),len(moNmc.MsgChan))
		select {
		case <-s.ExitSrv:
			s.Logger.Debug().Msgf("账号(%s) 收到s.ExitSrv信号,退出consumeDeliverMsg", s.RunID)
			exitFlag = true
		default:
		}

		if exitFlag {
			if atomic.LoadInt32(&deliverNmc.StopFlag) == 0 {
				deliverNmc.Stop()
				s.Logger.Info().Msgf("通道(%s) 已关闭 deliverNmc.Consumer", s.RunID)
			}
			if atomic.LoadInt32(&moNmc.StopFlag) == 0 {
				moNmc.Stop()
				s.Logger.Info().Msgf("通道(%s) 已关闭 moNmc.Consumer", s.RunID)
			}
		}

		select {
		case deliverMsg := <-deliverNmc.MsgChan:
			//fix me
			if err = snd.msgWrite(ctx, deliverMsg.Body, 1); err != nil {
				s.Logger.Error().Msgf("账号(%s) deliverMsg return error: %v", s.RunID, err)
				s.Logger.Debug().Msgf("通道(%s) deliverMsg.Body: %v", s.RunID, deliverMsg.Body)
				exitFlag = true
				topicName := deliverNmc.TopicName
				if err = models.Prn.PubMgr.Publish(topicName, deliverMsg.Body); err != nil {
					logger.Error().Msgf("账号(%s) models.Prn.PubMgr.Publish error:%v, topicName: %s", s.RunID, err, topicName)
				}
				s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", s.RunID)
				utils.CloseChan(&s.ExitSrv, s.mutex)
			}
		case moMsg := <-moNmc.MsgChan:
			//fix me
			if err = snd.msgWrite(ctx, moMsg.Body, 0); err != nil {
				s.Logger.Error().Msgf("账号(%s) moMsg return error: %v", s.RunID, err)
				s.Logger.Debug().Msgf("通道(%s) moMsg.Body: %v", s.RunID, moMsg.Body)
				exitFlag = true
				topicName := moNmc.TopicName
				if err = models.Prn.PubMgr.Publish(topicName, moMsg.Body); err != nil {
					logger.Error().Msgf("账号(%s) models.Prn.PubMgr.Publish error:%v, topicName: %s", s.RunID, err, topicName)
				}
				s.Logger.Debug().Msgf("通道(%s) close(c.ExitSrv)", s.RunID)
				utils.CloseChan(&s.ExitSrv, s.mutex)
			}
		case msg := <-s.deliverFakeChan:
			if err = snd.msgWrite(ctx, msg, 1); err != nil {
				s.Logger.Error().Msgf("账号(%s) msg return error: %v", s.RunID, err)
				exitFlag = true
				utils.CloseChan(&s.ExitSrv, s.mutex)
			}
		case <-timer.C:
			//s.Logger.Debug().Msgf("账号(%s) consumeDeliverMsg Tick at: %v", s.RunID, t)
			if exitFlag {
				s.Logger.Error().Msgf("账号(%s) exitFlag is true, 退出", s.RunID)
				goto EXIT
			}
		}
	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting deliverMsg...", s.RunID)
}

func (snd *deliverSender) cleanChan(msg chan nsq.Message) {
	s := snd.s
	s.Logger.Info().Msgf("账号(%s) 开始清理chan缓存", s.RunID)
	for {
		if len(msg) == 0 {
			break
		}
		select {
		case m := <-msg:
			s.Logger.Info().Msgf("账号(%s) record :%v", m)
		default:
			break
		}
	}
	s.Logger.Info().Msgf("账号(%s) chan缓存已完成清理", s.RunID)
}

func (snd *deliverSender) msgWrite(ctx context.Context, msg []byte, registerDelivery uint8) error {
	s := snd.s
	dm := &cmpp.DeliverMsg{}
	var msgID uint64
	var destID, srcTerminalID *common.OctetString
	var content []byte
	if registerDelivery == 0 { //上行
		p := &cmpp.MoMsgInfo{}
		err := json.Unmarshal(msg, p)
		if err != nil {
			s.Logger.Error().Msgf("账号(%s) json.unmarshal error:%v", s.RunID, err)
			return err
		}
		msgID = <-utils.MsgIdChan
		if msgID == 0 {
			s.Logger.Error().Msgf("账号(%s) msgId generate error:", s.RunID)
			return errors.New("msgId generate error")
		}
		//s.Logger.Debug().Msgf("MoMsgInfo:%v", p)
		content = utils.Utf8ToUcs2([]byte(p.MessageInfo))
		destID = &common.OctetString{Data: []byte(s.Account.CmppDestID + p.DevelopNo), FixedLen: 21}
		srcTerminalID = &common.OctetString{Data: []byte(p.Mobile), FixedLen: 21}
	} else if registerDelivery == 1 { // 回执状态报告
		dmi := &cmpp.DeliverMsgInfo{}
		err := json.Unmarshal(msg, dmi)
		if err != nil {
			s.Logger.Error().Msgf("账号(%s) json.unmarshal error:%v", s.Account.NickName, err)
			s.Logger.Error().Msgf("dmi msg json: %v", msg)
			return err
		}
		msgID, err = strconv.ParseUint(dmi.MsgId, 10, 64)
		if err != nil {
			s.Logger.Error().Msgf("dmi.MsgId(%s) parseUint error:%v", dmi.MsgId, err)
		}

		t, err := time.Parse("2006-01-02 15:04:05", dmi.SendTime)
		if err != nil {
			s.Logger.Error().Msgf("time.parse: %v", err)
		}
		sendTime := t.Format("0601021504")

		dm.MsgId = msgID
		dm.Stat = &common.OctetString{Data: []byte(dmi.StatusMessage), FixedLen: 7}
		dm.DestTerminalId = &common.OctetString{Data: []byte(dmi.Mobile), FixedLen: 21}
		dm.SubmitTime = &common.OctetString{Data: []byte(sendTime), FixedLen: 10}
		dm.DoneTime = &common.OctetString{Data: []byte(sendTime), FixedLen: 10}
		dm.SmscSequence = 0

		destID = &common.OctetString{Data: []byte(s.Account.CmppJoinDestID), FixedLen: 21}
		srcTerminalID = &common.OctetString{Data: []byte(dmi.Mobile), FixedLen: 21}
		content = dm.Serialize()
	} else {
		s.Logger.Debug().Msgf("registerDelivery error: %d", registerDelivery)
		return nil
	}
	d := &cmpp.Deliver{}
	d.MsgId = msgID
	d.DestId = destID
	d.ServiceId = &common.OctetString{Data: []byte(s.Account.NickName), FixedLen: 10}
	d.TPPid = 1
	d.TPUdhi = 0
	d.MsgFmt = 8
	d.SrcTerminalId = srcTerminalID
	d.RegisteredDelivery = registerDelivery
	tLen := len(content)
	d.MsgLength = uint8(tLen)
	d.MsgContent = content
	d.Reserve = &common.OctetString{Data: []byte(""), FixedLen: 8}
	newSeqID := atomic.AddUint32(&SeqID, 1)
	d.SeqId = newSeqID
	d.CmdId = common.CMPP_DELIVER
	d.TotalLen = 12 + 8 + 21 + 10 + 1 + 1 + 1 + 21 + 1 + 1 + uint32(tLen) + 8

	if err := d.IOWrite(s.rw); err != nil {
		s.Logger.Error().Msgf("账号(%s) send deliver msg error:%v", s.RunID, err)
		return err
	}
	mapKey := strconv.Itoa(int(s.Account.ID)) + ":" + strconv.Itoa(int(newSeqID))
	s.deliverMsgMap.Set(mapKey, *d)
	select {
	case s.mapKeyInChan <- mapKey: // 仅用作回执发送缓冲控制
	case <-ctx.Done():
		s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 deliverRespMsg 协程....", s.RunID)
		return nil
	}

	s.DeliverSendCount++
	if s.DeliverSendCount%utils.PeekInterval == 0 {
		s.Logger.Debug().Msgf("账号(%s) 推送回执，SeqId: %d, s.DeliverSendCount: %d, s.deliverMsgMap.Count:%d,"+
			" registerDelivery: %d", s.RunID, d.SeqId, s.DeliverSendCount, s.deliverMsgMap.Count(), registerDelivery)
	}
	return nil
}

func (snd *deliverSender) handleDeliverResp(ctx context.Context) {
	s := snd.s
	runID := s.RunID
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 deliverRespMsg 协程....", runID)
			return
		case resp := <-s.deliverRespChan:
			seqID := resp.SeqId
			mapKey := strconv.Itoa(int(s.Account.ID)) + ":" + strconv.Itoa(int(seqID))
			if resp.Result != 0 {
				s.Logger.Error().Msgf("账号(%s) deliver Resp.Result(%d) != 0,resp: %v", s.RunID, resp.Result, resp)
				var count int
				var d cmpp.Deliver
				if tmp, ok := s.deliverMsgMap.Get(mapKey); ok {
					d = tmp.(cmpp.Deliver)
					if tmp, ok := s.deliverResendCountMap.Get(mapKey); ok {
						count = tmp.(int)
						count++
					} else {
						count = 1
					}
					if count < 3 {
						s.deliverResendCountMap.Set(mapKey, count)
						if err := d.IOWrite(s.rw); err != nil {
							s.Logger.Error().Msgf("账号(%s) resend deliver msg error:%v,count:%d",
								s.RunID, err, count)
							return
						}
						s.Logger.Warn().Msgf("账号(%s) resend deliver msg, mapInKey:%s,count:%d",
							s.RunID, mapKey, count)
					} else {
						select {
						case <-s.mapKeyInChan:
						default:
						}
						s.deliverMsgMap.Remove(mapKey)
						s.deliverResendCountMap.Remove(mapKey)
					}
				}
			} else {
				select {
				case <-s.mapKeyInChan:
					if _, ok := s.deliverMsgMap.Get(mapKey); ok {
						s.deliverMsgMap.Remove(mapKey)
					}
					if _, ok := s.deliverResendCountMap.Get(mapKey); ok {
						s.deliverResendCountMap.Remove(mapKey)
					}
				default:
				}
			}
		case <-timer.C:
			//s.Logger.Debug().Msgf("账号(%s) s.deliverRespChan Tick at: %v", s.RunID, t)
		}
	}
}
