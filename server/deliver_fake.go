package server

import (
	"encoding/json"
	"sms_lib/utils"
	"strconv"
)

func (s *SrvConn) makeDeliverMsg(msgId uint64) {
	runId := s.RunId
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
			logger.Error().Msgf("帐号(%s) error marshal json:%v", runId, err)
		}
	}
	if registerDelivery == 1 {
		//select {
		//case s.delverMoChan <- messageBody:
		//default:
		//}
		//fmt.Println("deliverMsg topicName,", topicName)
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
			logger.Error().Msgf("帐号(%s) error marshal json:%v", runId, err)
		}
	}
	select {
	case s.deliverFakeChan <- b:
	default:
	}
}
