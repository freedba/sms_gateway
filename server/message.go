package server

import (
	"context"
	"encoding/json"
	"math"
	"sms_lib/models"
	"sms_lib/protocol/cmpp"
	"sms_lib/protocol/common"
	"sms_lib/utils"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

func SubmitMsgIDToQueue(s *SrvConn) {
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.waitGroup.Wrap(func() { SubmitMsgIDToDB(ctx, s) }) //nsqd集群失败处理协程

	for {
		utils.ResetTimer(timer, utils.Timeout)

		if s.IsClosing() && len(s.SubmitChan) == 0 {
			//网络关闭，nsq可用处理完缓存数据再退出.
			goto EXIT
		}
		select {
		case p := <-s.SubmitChan:
			s.waitGroup.Wrap(func() {
				smsAssemble(p, s)
			})
		case <-timer.C:
			//logger.Debug().Msgf("账号(%s) SubmitMsgIdToQueue Tick at", s.RunId)
		}
	}
EXIT:
	s.Logger.Debug().Msgf("账号(%s) Exiting SubmitMsgIdToQueue...", s.RunID)
}

func smsAssemble(p *cmpp.Submit, s *SrvConn) {
	flag := false
	var sendMsgID []string
	var content []byte
	msgID := strconv.FormatUint(p.MsgId, 10)
	if p.TPUdhi == 1 { //长短信
		byte4 := p.MsgContent[0:6][3]
		pkTotal := p.PkTotal
		if utils.Debug {
			s.Logger.Debug().Msgf("udhi:%v,byte4:%d", p.MsgContent[0:6], byte4)
		}
		s.lsmLock.Lock() // 保证一个协程组合长短信
		ls := s.lsm.get(byte4)
		if ls != nil && ls.len() == pkTotal {
			for i := uint8(1); i <= pkTotal; i++ {
				ID, buf := ls.get(i)
				sendMsgID = append(sendMsgID, ID)
				content = append(content, buf...)
			}
			s.lsm.del(byte4)
			flag = true
			if utils.Debug {
				s.Logger.Debug().Msgf("组合成长短信msgID：%s,s.longSms.len:%d", sendMsgID, s.lsm.len())
			}
		}
		s.lsmLock.Unlock()

		if utils.Debug {
			s.Logger.Debug().Msgf("拆分的短信msgID：%s", msgID)
		}
	} else if p.TPUdhi == 0 { //普通短信
		content = p.MsgContent
		sendMsgID = append(sendMsgID, msgID)
		flag = true
	}

	if flag {
		hsm := &HTTPSubmitMessageInfo{}
		var destTerminalID []string
		if p.MsgFmt == common.UCS2 {
			content = utils.Ucs2ToUtf8(content)
		} else if p.MsgFmt == common.GB18030 {
			content = utils.GbkToUtf8(content)
		} else {
			s.Logger.Error().Msgf("p.MsgFmt: %v", p.MsgFmt)

		}
		if utils.Debug {
			s.Logger.Debug().Msgf("长短信内容：%s", string(content))
		}
		hsm.TaskContent = string(content)
		hsm.DevelopNo = p.SrcId.String()[len(s.Account.CmppDestID):]
		for _, v := range p.DestTerminalId {
			destTerminalID = append(destTerminalID, v.String())
		}
		hsm.MobileContent = strings.Join(destTerminalID, ",")
		hsm.SendMsgID = strings.Join(sendMsgID, ",")
		hsm.Wrapper(s)
		count := atomic.AddInt64(&s.SubmitToQueueCount, 1)
		if count%int64(utils.PeekInterval) == 0 {
			s.Logger.Debug().Msgf("账号(%s) 提交消息入队列，SeqId: %d, sendMsgId: %s, s.SubmitChan len: %d,"+
				"s.SubmitToQueueCount: %d, ", s.RunID, p.SeqId, sendMsgID, len(s.SubmitChan), count)
			s.Logger.Debug().Msgf("长短信：s.longSms.len:%d", s.lsm.len())
		}
	}
}

func (hsm *HTTPSubmitMessageInfo) Wrapper(s *SrvConn) {
	var topicName string
	var freeTrial int64
	discard := true
	timer := time.NewTimer(utils.Timeout)
	defer timer.Stop()

	hsm.UID = s.Account.ID
	businessID := s.Account.BusinessID
	if businessID == 5 {
		topicName = "nsq.httpmarketing.submit.process"
		freeTrial = s.Account.MarketFreeTrial

	} else {
		topicName = "nsq.httpbusiness.submit.process"
		freeTrial = s.Account.FreeTrial
	}

	//s.Logger.Debug().Msgf("s.Account :%v,s.Account.BusinessInfo:%v", s.Account, s.Account.BusinessInfo)
	for _, v := range s.Account.BusinessInfo {
		//s.Logger.Debug().Msgf("v:%v,v.BusinessId:%v,businessId:%v, v.Status:%v", v, v.BusinessId, businessId, v.Status)
		if v.BusinessID == businessID {
			if v.Status != 1 {
				break
			}
			discard = false
			hsm.Deduct = v.Deduct

			if v.YidongChannelID != 0 && v.LiantongChannelID != 0 && v.DianxinChannelID != 0 {
				hsm.YidongChannelID = v.YidongChannelID
				hsm.LiantongChannelID = v.LiantongChannelID
				hsm.DianxinChannelID = v.DianxinChannelID
				hsm.SendStatus = 2
			} else {
				hsm.YidongChannelID = 0
				hsm.LiantongChannelID = 0
				hsm.DianxinChannelID = 0
				hsm.SendStatus = 1
			}
		}
	}
	if discard { //短信丢弃
		s.Logger.Error().Msgf("丢弃的短信:%v", hsm.SendMsgID)
		return
	}
	sendLen := int64(len([]rune(hsm.TaskContent)))
	hsm.RealNum = 1
	if sendLen > 70 {
		hsm.RealNum = int64(math.Ceil(float64(sendLen) / float64(67)))
	}
	hsm.FreeTrial = freeTrial
	if hsm.FreeTrial == 2 {
		hsm.SendStatus = 2
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
	if err := hsm.enQueue(topicName, s.RunID); err != nil {
		select {
		case s.hsmChan <- *hsm: //入nsq失败后入mysql
		case t := <-timer.C:
			s.Logger.Debug().Msgf("账号(%s) 写管道 s.HsmChan 超时, Tick at %v", s.RunID, t)
			s.Logger.Debug().Msgf("账号(%s) record hsm: %v ", s.RunID, hsm)
		}
	}
}

func (hsm HTTPSubmitMessageInfo) enQueue(topicName string, runID string) error {
	b, err := json.Marshal(hsm)
	if err != nil {
		logger.Error().Msgf("账号(%s) json.Marshal error:", runID, err)
		return err
	}

	if err = models.Prn.PubMgr.Publish(topicName, b); err != nil {
		logger.Error().Msgf("账号(%s) models.Prn.PubMgr.Publish error:%v, topicName: %s", runID, err, topicName)
		return err
	}
	return nil
}

type arrMsgs []HTTPSubmitMessageInfo

func (a arrMsgs) batchCreate(s *SrvConn) {
	var tbName1 string
	var tbName2 string

	if s.Account.BusinessID == 5 { //营销
		tbName1 = "yx_user_send_task_fornsqd"
		tbName2 = "yx_user_send_task_content_fornsqd"
	} else {
		tbName1 = "yx_user_send_code_task_fornsqd"
		tbName2 = "yx_user_send_code_task_content_fornsqd"
	}

	t1, t2 := a.BatchHandle()
	if err := models.DB.Table(tbName1).Create(&t1).Error; err != nil {
		s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunID, tbName1, err)
		s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunID, tbName1, t1)
	}
	//logger.Debug().Msgf("通道%v,入库成功记录：%d", t1,len(t1))
	if err := models.DB.Table(tbName2).Create(&t2).Error; err != nil {
		s.Logger.Error().Msgf("账号(%s) db insert table (%s) error:%v", s.RunID, tbName2, err)
		s.Logger.Error().Msgf("账号(%s) record table(%s) : %v ", s.RunID, tbName2, t2)
	}
	//logger.Debug().Msgf("通道%v,入库成功记录：%d", t2,len(t2))
}

func (a arrMsgs) BatchHandle() ([]models.Yx_user_send_task_fronsqd, []models.Yx_user_send_task_content_fornsqd) {
	var t1 []models.Yx_user_send_task_fronsqd
	var t2 []models.Yx_user_send_task_content_fornsqd

	for _, msg := range a {
		// logger.Debug().Msgf("-----msg:",msg)
		yxUserSendCodeTaskFroNsqd := models.Yx_user_send_task_fronsqd{
			Task_no:             msg.TaskNo,
			Uid:                 msg.UID,
			Send_msg_id:         msg.SendMsgID,
			Source:              msg.Source,
			Real_num:            msg.RealNum,
			Send_num:            msg.SendNum,
			Send_length:         msg.SendLength,
			Free_trial:          msg.FreeTrial,
			Develop_no:          msg.DevelopNo,
			Yidong_channel_id:   msg.YidongChannelID,
			Liantong_channel_id: msg.LiantongChannelID,
			Dianxin_channel_id:  msg.DianxinChannelID,
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
		t1 = append(t1, yxUserSendCodeTaskFroNsqd)

		yxUserSendTaskContentForNsqd := models.Yx_user_send_task_content_fornsqd{
			Task_no:        msg.TaskNo,
			Task_content:   msg.TaskContent,
			Mobile_content: msg.MobileContent,
			Create_time:    msg.SubmitTime,
		}
		t2 = append(t2, yxUserSendTaskContentForNsqd)
	}
	return t1, t2
}

func SubmitMsgIDToDB(ctx context.Context, s *SrvConn) {
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
			s.Logger.Debug().Msgf("账号(%s) 接收到 ctx.Done() 退出信号，退出 SubmitMsgIdToDB 协程....", s.RunID)
			return
		case <-timer.C:
			//logger.Debug().Msgf("账号(%s) SubmitMsgIdToDB Tick at: %v", s.RunId, t)
			todb = true
		}
	}
}
