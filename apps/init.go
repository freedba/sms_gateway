package main

import (
	"reflect"
	"sms_lib/levellogger"
)

var logger = levellogger.LLogger

func init() {
	type Empty struct{}
	pkgName := reflect.TypeOf(Empty{}).PkgPath()
	logger.Info().Msgf("pkgName %s init zerolog", pkgName)
}
