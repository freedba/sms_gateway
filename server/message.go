package server

import (
	"context"
	"encoding/json"
	"math"
	"sms_lib/models"
	"sms_lib/protocol/common"
	"sms_lib/utils"
	"strconv"
	"strings"
	"sync"
	"time"
)

func SubmitMsgIdToQueue(s *SrvConn) {
	flag := false
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	var sendMsgId []string
	var content []byte
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { SubmitMsgIdToDB(ctx, s) }) //nsqd集群失败处理协程

	for {
		utils.ResetTimer(timer, utils.Timeout)

		if s.IsClosing() && len(s.SubmitChan) == 0 {
			//网络关闭，nsq可用处理完缓存数据再退出.
			goto EXIT
		}
		select {
		case p := <-s.SubmitChan:
			msgId := strconv.FormatUint(p.MsgId, 10)
			if p.TPUdhi == 1 { //长短信
				udhi := p.MsgContent[0:6]
				rand := udhi[3]
				if !s.longSms.exist(rand) {
					ls := &LongSms{
						Content: make(map[uint8][]byte),
						MsgID:   make(map[uint8]string),
						mLock:   new(sync.Mutex),
					}
					s.longSms.set(rand, ls)
				}
				pkTotal := p.PkTotal
				pkNumber := p.PkNumber
				ls := s.longSms.get(rand)
				ls.set(pkNumber, msgId, p.MsgContent[6:])
				if ls.len() == pkTotal {
					for i := uint8(1); i <= pkTotal; i++ {
						ID, buf := ls.get(i)
						sendMsgId = append(sendMsgId, ID)
						content = append(content, buf...)
					}
					s.longSms.del(rand)
					flag = true
				}
				s.Logger.Debug().Msgf("拆分的短信msgID：%s", msgId)
			} else if p.TPUdhi == 0 { //短短信
				content = p.MsgContent
				sendMsgId = append(sendMsgId, msgId)
				flag = true
			}

			if flag {
				hsm := &HttpSubmitMessageInfo{}
				var destTerminalId []string
				if p.MsgFmt == common.UCS2 {
					content = utils.Ucs2ToUtf8(content)
				} else if p.MsgFmt == common.GB18030 {
					content = utils.GbkToUtf8(content)
				} else {

				}
				hsm.TaskContent = string(content)
				hsm.DevelopNo = p.SrcId.String()[len(s.Account.CmppDestId):]
				for _, v := range p.DestTerminalId {
					destTerminalId = append(destTerminalId, v.String())
				}
				hsm.MobileContent = strings.Join(destTerminalId, ",")
				hsm.SendMsgId = strings.Join(sendMsgId, ",")
				//logger.Debug().Msgf("p.PkTotal:%d,sendMsgId:%s,hsm.MobileContent:%s,p.DestTerminalId:%v",
				//	p.PkTotal, sendMsgId, hsm.MobileContent, p.DestTerminalId)
				hsm.Wrapper(s)
				s.SubmitToQueueCount++
				if s.SubmitToQueueCount%utils.PeekInterval == 0 {
					s.Logger.Debug().Msgf("账号(%s) 提交消息入队列，SeqId: %d, MsgId: %s, s.SubmitChan len: %d,"+
						"s.SubmitToQueueCount: %d, ", s.RunId, p.SeqId, sendMsgId, len(s.SubmitChan), s.SubmitToQueueCount)
					s.Logger.Debug().Msgf("未处理完的长短信：s.longsms.len:%d,s.longsms:%+v", s.longSms.len(), s.longSms)
					//logger.Debug().Msgf("hsm.DevelopNo:%s",hsm.DevelopNo)
				}
				flag = false
				sendMsgId = nil
				content = nil
				s.Logger.Debug().Msgf("组合成长短信msgID：%s", sendMsgId)
			}
		case <-timer.C:
			//logger.Debug().Msgf("账号(%s) SubmitMsgIdToQueue Tick at", s.RunId)
		}
	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting SubmitMsgIdToQueue...", s.RunId)
}

func (hsm *HttpSubmitMessageInfo) Wrapper(s *SrvConn) {
	var topicName string
	discard := true
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
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
			if v.Status != 1 {
				break
			}
			discard = false
			hsm.Deduct = v.Deduct
			if v.YidongChannelId != 0 && v.LiantongChannelId != 0 && v.DianxinChannelId != 0 {
				hsm.YidongChannelId = v.YidongChannelId
				hsm.LiantongChannelId = v.LiantongChannelId
				hsm.DianxinChannelId = v.DianxinChannelId
				hsm.SendStatus = 2
				hsm.FreeTrial = 2
			} else {
				hsm.YidongChannelId = 0
				hsm.LiantongChannelId = 0
				hsm.DianxinChannelId = 0
				hsm.FreeTrial = 1
				hsm.SendStatus = 1
			}
		}
	}
	if discard { //短信丢弃
		logger.Error().Msgf("丢弃的短信:%v", hsm.SendMsgId)
		return
	}
	sendLen := int64(len([]rune(hsm.TaskContent)))
	hsm.RealNum = 1
	if sendLen > 70 {
		hsm.RealNum = int64(math.Ceil(float64(sendLen) / float64(67)))
	}
	hsm.IsneedReceipt = s.Account.IsNeedReceipt
	hsm.NeedReceiptType = s.Account.NeedReceiptType
	hsm.IsHaveSelected = s.Account.IsHaveSelected
	hsm.SendNum = 1
	hsm.From = 2
	hsm.SendLength = sendLen
	hsm.AppointmentTime = 0
	hsm.Extra = `{"from":"cmppserver"}`
	hsm.TaskNo = utils.GetUuid()
	hsm.SubmitTime = time.Now().Unix()
	err := hsm.enQueue(topicName, s.RunId)
	if err != nil {
		select {
		case s.hsmChan <- *hsm: //入nsq失败后入mysql
		case t := <-timer.C:
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

	t1, t2 := a.BatchHandle()
	if err := models.DB.Table(tbName1).Create(&t1).Error; err != nil {
		s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunId, tbName1, err)
		s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunId, tbName1, t1)
	}
	//logger.Debug().Msgf("通道%v,入库成功记录：%d", t1,len(t1))
	if err := models.DB.Table(tbName2).Create(&t2).Error; err != nil {
		s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunId, tbName2, err)
		s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunId, tbName2, t2)
	}
	//logger.Debug().Msgf("通道%v,入库成功记录：%d", t2,len(t2))
}

func (a arrMsgs) BatchHandle() ([]models.Yx_user_send_task_fronsqd, []models.Yx_user_send_task_content_fornsqd) {
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
	defer timer.Stop()
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

//type DeliverMsgInfo struct {
//	TaskNo        string `json:"task_no"`
//	StatusMessage string `json:"status_message"`
//	MessageInfo   string `json:"message_info"`
//	Mobile        string `json:"mobile"`
//	MsgId         string `json:"msg_id"`
//	SendTime      string `json:"send_time"`
//}
//
//type MoMsgInfo struct {
//	Mobile      string `json:"mobile"`
//	MessageInfo string `json:"message_info"`
//	BusinessId  int    `json:"business_id"`
//	GetTime     string `json:"get_time"`
//	DevelopNo   string `json:"develop_no"`
//}
