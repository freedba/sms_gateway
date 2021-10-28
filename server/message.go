package server

import (
	"context"
	"encoding/json"
	"errors"
	"sms_lib/config"
	"sms_lib/models"
	"sms_lib/protocol"
	"sms_lib/utils"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func SubmitMsgIdToQueue(s *SrvConn) {
	flag := false
	timer := time.NewTimer(utils.Timeout)
	var sendMsgId []string
	var content []byte
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { SubmitMsgIdToDB(ctx, s) }) //nsqd集群失败处理协程

	for {
		utils.ResetTimer(timer, utils.Timeout)

		if s.IsClosing() && len(s.SubmitChan) == 0 {
			//1,网络关闭，nsq可用处理完缓存数据再退出.
			//2,nsq不可用直接退出
			goto EXIT
		}
		select {
		case p := <-s.SubmitChan:
			if p.TPUdhi == 1 { //长短信
				udhi := p.MsgContent[0:6]
				rand := udhi[3]
				if _, ok := s.longSms[rand]; !ok {
					s.longSms = make(map[uint8]map[uint8][]byte)
					s.longSms[rand] = make(map[uint8][]byte)
				}
				pkTotal := p.PkTotal
				pkNumber := p.PkNumber
				s.longSms[rand][pkNumber] = p.MsgContent[6:]
				if len(s.longSms[rand]) == int(pkTotal) {
					for i := uint8(1); i <= pkTotal; i++ {
						content = append(content, s.longSms[rand][i]...)
					}
					flag = true
				}
			} else if p.TPUdhi == 0 { //短短信
				content = p.MsgContent
				flag = true
			}

			msgId := strconv.FormatUint(p.MsgId, 10)
			sendMsgId = append(sendMsgId, msgId)
			if flag {
				hsm := &HttpSubmitMessageInfo{}
				var destTerminalId []string
				if p.MsgFmt == 8 {
					content = utils.Ucs2ToUtf8(content)
				} else if p.MsgFmt == 15 {
					content = utils.GbkToUtf8(content)
				}
				hsm.TaskContent = string(content)
				hsm.DevelopNo = strings.TrimSpace(string(p.SrcId.Data[len(s.Account.CmppDestId):]))
				logger.Debug().Msgf("hsm.DevelopNo:%s", hsm.DevelopNo)
				for _, v := range p.DestTerminalId {
					destTerminalId = append(destTerminalId, v.String())
				}
				hsm.MobileContent = strings.Join(destTerminalId, ",")
				hsm.SendMsgId = strings.Join(sendMsgId, ",")
				//p.MsgContent = content
				//logger.Debug().Msgf("content:%s,sendMsgId:%s",string(p.MsgContent),sendMsgId)
				hsm.Wrapper(s)
				s.SubmitToQueueCount++
				if s.SubmitToQueueCount%utils.PeekInterval == 0 {
					s.Logger.Debug().Msgf("账号(%s) 提交消息入队列，SeqId: %d, MsgId: %s, s.SubmitChan len: %d,"+
						"s.SubmitToQueueCount: %d", s.RunId, p.SeqId, sendMsgId, len(s.SubmitChan), s.SubmitToQueueCount)
					//logger.Debug().Msgf("hsm.DevelopNo:%s",hsm.DevelopNo)
				}
				flag = false
				sendMsgId = nil
				content = nil
				s.longSms = nil
			}
		case <-timer.C:
			//logger.Debug().Msgf("账号(%s) SubmitMsgIdToQueue Tick at: %v", s.RunId, t)
		}
	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting SubmitMsgIdToQueue...", s.RunId)
}

func (hsm *HttpSubmitMessageInfo) Wrapper(s *SrvConn) {
	var topicName string
	//developCode := string(p.SrcId.Data[len(s.Account.CmppDestId):])
	//hsm := &HttpSubmitMessageInfo{}
	hsm.Uid = s.Account.Id
	businessId := s.Account.BusinessId
	if businessId == 5 {
		topicName = "nsq.httpmarketing.submit.process"
	} else {
		topicName = "nsq.httpbusiness.submit.process"
	}
	for _, v := range s.Account.BusinessInfo {
		if v.BusinessId == businessId {
			hsm.Deduct = v.Deduct
			if v.Status == 1 {
				hsm.SendStatus = v.Status
				hsm.YidongChannelId = 0
				hsm.DianxinChannelId = 0
				hsm.LiantongChannelId = 0
				hsm.FreeTrial = 1
			} else {
				if v.YidongChannelId != 0 && v.LiantongChannelId != 0 && v.DianxinChannelId != 0 {
					hsm.YidongChannelId = v.YidongChannelId
					hsm.LiantongChannelId = v.LiantongChannelId
					hsm.DianxinChannelId = v.DianxinChannelId
					hsm.SendStatus = 2
					hsm.FreeTrial = 2
				} else {
					hsm.SendStatus = 1
					hsm.YidongChannelId = 0
					hsm.LiantongChannelId = 0
					hsm.DianxinChannelId = 0
					hsm.FreeTrial = 1
				}
			}
			break
		}
	}
	sendLen := int64(len(utils.Utf8ToUcs2([]byte(hsm.TaskContent))))
	hsm.IsneedReceipt = s.Account.IsNeedReceipt
	hsm.NeedReceiptType = s.Account.NeedReceiptType
	hsm.IsHaveSelected = s.Account.IsHaveSelected
	hsm.RealNum = 1
	hsm.SendNum = 1
	hsm.From = 2
	hsm.SendLength = sendLen
	hsm.AppointmentTime = 0
	//hsm.SendMsgId = strings.Join(sendMsgId, ",")
	hsm.Extra = `{"from":"cmppserver"}`
	hsm.TaskNo = utils.GetUuid()
	hsm.SubmitTime = time.Now().Unix()
	err := hsm.enQueue(topicName, s.RunId)
	if err != nil {
		select {
		case s.hsmChan <- *hsm: //入nsq失败后入mysql
		case t := <-time.After(utils.Timeout):
			s.Logger.Debug().Msgf("账号(%s) 写管道 s.HsmChan 超时, Tick at %v", s.RunId, t)
			s.Logger.Debug().Msgf("账号(%s) record hsm: %v ", s.RunId, hsm)
		}
	}
}

func (hsm HttpSubmitMessageInfo) enQueue(topicName string, runId string) error {
	b, err := json.Marshal(hsm)
	if err != nil {
		logger.Error().Msgf("账号(%s) json.Marshal error:", runId, err)
		return err
	}

	err = models.Prn.PubMgr.Publish(topicName, b)
	if err != nil {
		logger.Error().Msgf("账号(%s) models.Prn.PubMgr.Publish error:%v, topicName: %s", runId, err, topicName)
		return err
	}
	return nil
}

type arrMsgs []HttpSubmitMessageInfo

func (a arrMsgs) batchCreate(s *SrvConn) {
	var tbName1 string
	var tbName2 string

	if s.Account.BusinessId == 5 { //营销
		tbName1 = "yx_user_send_task_fornsqd"
		tbName2 = "yx_user_send_task_content_fornsqd"
	} else {
		tbName1 = "yx_user_send_code_task_fornsqd"
		tbName2 = "yx_user_send_code_task_content_fornsqd"
	}

	t1, t2 := a.GenBatchHandle()
	if len(t1) > 0 {
		table := models.Table{
			Name: tbName1,
		}
		if err := models.DB.Scopes(models.SetTable(table)).Create(&t1).Error; err != nil {
			s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunId, table, err)
			s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunId, table, t1)
		}
		//logger.Debug().Msgf("通道%v,入库成功记录：%d", t1,len(t1))
	}
	if len(t2) > 0 {
		table := models.Table{
			Name: tbName2,
		}
		if err := models.DB.Scopes(models.SetTable(table)).Create(&t2).Error; err != nil {
			s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunId, table, err)
			s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunId, table, t2)
		}
		//logger.Debug().Msgf("通道%v,入库成功记录：%d", t2,len(t2))
	}
}

func (a arrMsgs) GenBatchHandle() ([]models.Yx_user_send_task_fronsqd, []models.Yx_user_send_task_content_fornsqd) {
	var t1 []models.Yx_user_send_task_fronsqd
	var t2 []models.Yx_user_send_task_content_fornsqd

	for _, msg := range a {
		// logger.Debug().Msgf("-----msg:",msg)
		yx_user_send_code_task_fronsqd := models.Yx_user_send_task_fronsqd{
			Task_no:             msg.TaskNo,
			Uid:                 msg.Uid,
			Send_msg_id:         msg.SendMsgId,
			Source:              msg.Source,
			Real_num:            msg.RealNum,
			Send_num:            msg.SendNum,
			Send_length:         msg.SendLength,
			Free_trial:          msg.FreeTrial,
			Develop_no:          msg.DevelopNo,
			Yidong_channel_id:   msg.YidongChannelId,
			Liantong_channel_id: msg.LiantongChannelId,
			Dianxin_channel_id:  msg.DianxinChannelId,
			Send_status:         msg.SendStatus,
			Submit_time:         msg.SubmitTime,
			Isneed_receipt:      msg.IsneedReceipt,
			Need_receipt_type:   msg.NeedReceiptType,
			Is_have_selected:    msg.IsHaveSelected,
			From:                msg.From,
			Deduct:              msg.Deduct,
			Extra:               msg.Extra,
			Update_time:         time.Now().Unix(),
			Create_time:         time.Now().Unix(),
			Delete_time:         time.Now().Unix(),
			Appointment_time:    msg.AppointmentTime,
		}
		t1 = append(t1, yx_user_send_code_task_fronsqd)

		yx_user_send_task_content_fornsqd := models.Yx_user_send_task_content_fornsqd{
			Task_no:        msg.TaskNo,
			Task_content:   msg.TaskContent,
			Mobile_content: msg.MobileContent,
			Create_time:    msg.SubmitTime,
		}
		t2 = append(t2, yx_user_send_task_content_fornsqd)
	}
	return t1, t2
}

func SubmitMsgIdToDB(ctx context.Context, s *SrvConn) {
	var todb = false
	a := arrMsgs{}
	timer := time.NewTimer(utils.Timeout)
	for {
		utils.ResetTimer(timer, utils.Timeout)

		if len(a) > 50 || todb && len(a) > 0 {
			a.batchCreate(s)
			todb = false
			a = nil
		}
		select {
		case hsm := <-s.hsmChan:
			a = append(a, hsm)
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 SubmitMsgIdToDB 协程....", s.RunId)
			return
		case <-timer.C:
			//logger.Debug().Msgf("账号(%s) SubmitMsgIdToDB Tick at: %v", s.RunId, t)
			todb = true
		}
	}
}

type DeliverMsgInfo struct {
	TaskNo        string `json:"task_no"`
	StatusMessage string `json:"status_message"`
	MessageInfo   string `json:"message_info"`
	Mobile        string `json:"mobile"`
	MsgId         string `json:"msg_id"`
	SendTime      string `json:"send_time"`
}

type MoMsgInfo struct {
	Mobile      string `json:"mobile"`
	MessageInfo string `json:"message_info"`
	BusinessId  int    `json:"business_id"`
	GetTime     string `json:"get_time"`
	DevelopNo   string `json:"develop_no"`
}

type deliverSender struct {
	deliverNmc *models.NsqMsgChan
	moNmc      *models.NsqMsgChan
	wg         *sync.WaitGroup
	s          *SrvConn
	stopCh     chan struct{}
	mutex      *sync.Mutex
}

func DeliverPush(s *SrvConn) {
	cfg := config.GetTopicPrefix()
	chid := s.Account.Id
	user := s.Account.NickName
	var moNmc *models.NsqMsgChan
	var wg sync.WaitGroup
	var mutex = sync.Mutex{}
	var snd *deliverSender
	stopCh := make(chan struct{})

	topicPrefix := cfg.DeliverSend
	topicName := topicPrefix + strconv.FormatInt(chid, 10)
	deliverNmc, err := models.InitConsumer(topicPrefix, strconv.Itoa(int(chid)), 1)
	if err != nil {
		s.Logger.Error().Msgf("账号(%s) 启动消费 (%s) 失败: %v, Exiting DeliverSend...",
			user, topicName, err)
		goto EXIT
	}

	topicPrefix = cfg.DeliverMoSend
	topicName = topicPrefix + strconv.FormatInt(chid, 10)
	moNmc, err = models.InitConsumer(topicPrefix, strconv.Itoa(int(chid)), 1)
	if err != nil {
		s.Logger.Error().Msgf("账号(%s) 启动消费者 (%s) 失败: %v, Exiting DeliverSend...",
			user, topicName, err)
		goto EXIT
	}

	snd = &deliverSender{
		wg:         &wg,
		stopCh:     stopCh,
		s:          s,
		mutex:      &mutex,
		deliverNmc: deliverNmc,
		moNmc:      moNmc,
	}

	snd.consumeDeliverMsg()
	deliverNmc.Consumer.Stop()
	moNmc.Consumer.Stop()
EXIT:
	atomic.StoreInt32(&s.deliverSenderExit, 1)
	if atomic.LoadInt32(&s.ReadLoopRunning) == 1 {
		utils.ExitSig.DeliverySender[s.RunId] <- true
	}
	s.Logger.Debug().Msgf("账号(%s) Exiting DeliverSend...", s.RunId)
}

func (snd *deliverSender) consumeDeliverMsg() {
	s := snd.s
	var err error
	timer := time.NewTimer(utils.Timeout)
	deliverNmc := snd.deliverNmc
	moNmc := snd.moNmc
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { snd.handleDeliverResp(ctx) })

	for {
		utils.ResetTimer(timer, utils.Timeout)
		if s.IsClosing() || s.ReadLoopRunning == 0 {
			s.Logger.Debug().Msgf("账号(%s) s.IsClosing:%v,s.ReadLoopRunning:%d",
				s.RunId, s.IsClosing(), s.ReadLoopRunning)
			goto EXIT
		}
		//logger.Debug().Msgf("deliverNmc.MsgChan:%d,moNmc.MsgChan:%d",len(deliverNmc.MsgChan),len(moNmc.MsgChan))
		select {
		case <-utils.ExitSig.LoopRead[s.RunId]:
			s.Logger.Debug().Msgf("账号(%s) 收到utils.ExitSig.LoopRead信号,退出consumeDeliverMsg", s.RunId)
			goto EXIT
		case deliverMsg := <-deliverNmc.MsgChan:
			err = snd.msgWrite(1, deliverMsg.Body)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) deliverMsg return error: %v", s.RunId, err)
				goto EXIT
			}
		case moMsg := <-moNmc.MsgChan:
			err = snd.msgWrite(0, moMsg.Body)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) moMsg return error: %v", s.RunId, err)
				goto EXIT
			}
		case msg := <-s.deliverFakeChan:
			err = snd.msgWrite(1, msg)
			if err != nil {
				s.Logger.Error().Msgf("账号(%s) msg return error: %v", s.RunId, err)
				goto EXIT
			}
		case <-timer.C:
			//s.Logger.Debug().Msgf("账号(%s) consumeDeliverMsg Tick at: %v", s.RunId, t)
		}
	}
EXIT:
	close(deliverNmc.Stop)
	close(moNmc.Stop)
	s.Logger.Debug().Msgf("账号(%s) Exiting deliverMsg", s.RunId)
	return
}

func (snd *deliverSender) msgWrite(registerDelivery uint8, msg []byte) error {
	s := snd.s
	dm := &protocol.DeliverMsg{}
	var msgId uint64
	var destId, srcTerminalId *protocol.OctetString
	var content []byte
	if registerDelivery == 0 { //上行
		p := &MoMsgInfo{}
		err := json.Unmarshal(msg, p)
		if err != nil {
			s.Logger.Error().Msgf("账号(%s) json.unmarshal error:%v", s.RunId, err)
			return err
		}
		msgId = generateMsgID()
		if msgId == 0 {
			s.Logger.Error().Msgf("账号(%s) msgId generate error:", s.RunId)
			return errors.New("msgId generate error")
		}
		content = utils.Utf8ToUcs2([]byte(p.MessageInfo))
		destId = &protocol.OctetString{Data: []byte(s.Account.CmppDestId + p.DevelopNo), FixedLen: 21}
		srcTerminalId = &protocol.OctetString{Data: []byte(p.Mobile), FixedLen: 21}
	} else if registerDelivery == 1 { // 回执状态报告
		dmi := &DeliverMsgInfo{}
		//dmi := snd.deliverMsgInfoPool.Get().(*DeliverMsgInfo)
		err := json.Unmarshal(msg, dmi)
		if err != nil {
			s.Logger.Error().Msgf("账号(%s) json.unmarshal error:%v", s.Account.NickName, err)
			s.Logger.Error().Msgf("dmi msg json: %v", msg)
			return err
		}
		msgId, _ = strconv.ParseUint(dmi.MsgId, 10, 64)

		dm.MsgId = msgId
		dm.Stat = &protocol.OctetString{Data: []byte(dmi.StatusMessage), FixedLen: 7}
		dm.DestTerminalId = &protocol.OctetString{Data: []byte(dmi.Mobile), FixedLen: 21}
		dm.SubmitTime = &protocol.OctetString{Data: []byte(dmi.SendTime), FixedLen: 10}
		dm.DoneTime = &protocol.OctetString{Data: []byte(dmi.SendTime), FixedLen: 10}
		dm.SmscSequence = 0

		destId = &protocol.OctetString{Data: []byte(s.Account.CmppJoinDestId), FixedLen: 21}
		srcTerminalId = &protocol.OctetString{Data: []byte(dmi.Mobile), FixedLen: 21}
		content = dm.Serialize()
		//snd.moMsgInfoPool.Put(dmi)
	} else {
		s.Logger.Debug().Msgf("registerDelivery error: %d", registerDelivery)
		return nil
	}
	d := &protocol.Deliver{}
	d.MsgId = msgId
	d.DestId = destId
	d.ServiceId = &protocol.OctetString{Data: []byte(s.Account.NickName), FixedLen: 10}
	d.TPPid = 1
	d.TPUdhi = 0
	d.MsgFmt = 8
	d.SrcTerminalId = srcTerminalId
	d.RegisteredDelivery = registerDelivery
	tLen := len(content)
	d.MsgLength = uint8(tLen)
	d.MsgContent = content
	d.Reserve = &protocol.OctetString{Data: []byte(""), FixedLen: 8}
	newSeqId := atomic.AddUint32(&SeqId, 1)
	d.SeqId = newSeqId
	d.CmdId = protocol.CMPP_DELIVER
	d.TotalLen = 12 + 8 + 21 + 10 + 1 + 1 + 1 + 21 + 1 + 1 + uint32(tLen) + 8

	mapKey := strconv.Itoa(int(s.Account.Id)) + ":" + strconv.Itoa(int(newSeqId))
	s.mapKeyInChan <- mapKey // 仅用作回执发送缓冲控制
	s.deliverMsgMap.Set(mapKey, *d)
	if err := d.IOWrite(s.rw); err != nil {
		s.Logger.Error().Msgf("账号(%s) send deliver msg error:%v", s.RunId, err)
		return err
	}
	s.DeliverSendCount++
	if s.DeliverSendCount%utils.PeekInterval == 0 {
		s.Logger.Debug().Msgf("账号(%s) 推送回执，SeqId: %d, s.DeliverSendCount: %d, s.deliverMsgMap.Count:%d,"+
			" registerDelivery: %d", s.RunId, d.SeqId, s.DeliverSendCount, s.deliverMsgMap.Count(), registerDelivery)
	}
	return nil
}

func (snd *deliverSender) handleDeliverResp(ctx context.Context) {
	s := snd.s
	runId := s.RunId
	timer := time.NewTimer(utils.Timeout)
	for {
		select {
		case <-ctx.Done():
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 deliverRespMsg 协程....", runId)
			return
		case resp := <-s.deliverRespChan:
			seqId := resp.SeqId
			mapKey := strconv.Itoa(int(s.Account.Id)) + ":" + strconv.Itoa(int(seqId))
			if resp.Result != 0 {
				s.Logger.Error().Msgf("账号(%s) deliver Resp.Result(%d) != 0,resp: %v", s.RunId, resp.Result, resp)
				var count int
				var d protocol.Deliver
				if tmp, ok := s.deliverMsgMap.Get(mapKey); ok {
					d = tmp.(protocol.Deliver)
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
								s.RunId, err, count)
							return
						}
						s.Logger.Warn().Msgf("账号(%s) resend deliver msg, mapInKey:%s,count:%d",
							s.RunId, mapKey, count)
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
			//s.Logger.Debug().Msgf("账号(%s) s.deliverRespChan Tick at: %v", s.RunId, t)
		}
	}
}
