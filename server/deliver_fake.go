package server

import (
	"encoding/json"
	"sms_lib/protocol/cmpp"
	"sms_lib/protocol/common"
	"sms_lib/utils"
	"strconv"
	"strings"
	"time"
)

func (s *SrvConn) makeDeliverMsg(msgID uint64, destTerminalID []*common.OctetString) {
	runID := s.RunID
	registerDelivery := 1
	var err error
	var b []byte
	var mobile []string
	for _, v := range destTerminalID {
		mobile = append(mobile, v.String())
	}
	//hsm.MobileContent = strings.Join(mobile, ",")
	if registerDelivery == 0 {
		moMsg := cmpp.MoMsgInfo{
			Mobile:      "18432130952",
			MessageInfo: "content",
			BusinessId:  5,
			GetTime:     strconv.Itoa(int(utils.GetCurrTimestamp("ms"))),
			DevelopNo:   "6105",
		}
		b, err = json.Marshal(moMsg)
		if err != nil {
			s.Logger.Error().Msgf("帐号(%s) error marshal json:%v", runID, err)
		}
	}
	if registerDelivery == 1 {
		deliverMsg := cmpp.DeliverMsgInfo{
			Mobile:        strings.Join(mobile, ","),
			MessageInfo:   "content",
			SendTime:      strconv.Itoa(int(utils.GetCurrTimestamp("ms"))),
			TaskNo:        utils.GetUuid(),
			StatusMessage: "DELIVRD",
			MsgId:         strconv.FormatUint(msgID, 10),
		}
		b, err = json.Marshal(deliverMsg)
		if err != nil {
			s.Logger.Error().Msgf("帐号(%s) error marshal json:%v", runID, err)
		}
	}
	select {
	case s.deliverFakeChan <- b:
	case <-time.After(time.Duration(5) * time.Second):
		s.Logger.Warn().Msgf("写入管道失败, s.deliverFakeChan len: %d", len(s.deliverFakeChan))
	}
}
