package server

import (
	"reflect"
	"sms_lib/levellogger"
	"sms_lib/models"
	"sms_lib/utils"
)

var logger = levellogger.LLogger

func init() {
	type Empty struct{}
	pkgName := reflect.TypeOf(Empty{}).PkgPath()
	logger.Info().Msgf("pkgName %s init zerolog", pkgName)
}

func InitNodeId() {
	levellogger.NodeId = models.GetNodeId(runMode)
	utils.NodeId = levellogger.NodeId
}

func InitSeqId() uint32 {
	return uint32(utils.NodeId * 1000000)
}
