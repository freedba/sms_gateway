package server

import (
	"encoding/json"
	"sms_lib/utils"
	"strconv"
	"time"
)

func (s *SrvConn) makeDeliverMsg(msgId uint64) {
	runId := s.RunId
	//s.Logger.Info().Msgf("账号(%s) 启动虚拟网关",runId)
	registerDelivery := 1
	var err error
	var b []byte
	if registerDelivery == 0 {
		moMsg := MoMsgInfo{
			Mobile:      "18432130952",
			MessageInfo: "content",
			BusinessId:  5,
			GetTime:     strconv.Itoa(int(utils.GetCurrTimestamp("ms"))),
			DevelopNo:   "6105",
		}
		b, err = json.Marshal(moMsg)
		if err != nil {
			s.Logger.Error().Msgf("帐号(%s) error marshal json:%v", runId, err)
		}
	}
	if registerDelivery == 1 {
		deliverMsg := DeliverMsgInfo{
			Mobile:        "18432130952",
			MessageInfo:   "content",
			SendTime:      strconv.Itoa(int(utils.GetCurrTimestamp("ms"))),
			TaskNo:        utils.GetUuid(),
			StatusMessage: "DELIVRD",
			MsgId:         strconv.FormatUint(msgId, 10),
		}
		b, err = json.Marshal(deliverMsg)
		if err != nil {
			s.Logger.Error().Msgf("帐号(%s) error marshal json:%v", runId, err)
		}
	}
	select {
	case s.deliverFakeChan <- b:
	case <-time.After(time.Duration(1) * time.Second):
		s.Logger.Warn().Msgf("写入管道失败, s.deliverFakeChan len: %d", len(s.deliverFakeChan))
	}
}
