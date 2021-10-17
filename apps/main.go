package main

import (
	"flag"
	"github.com/pkg/profile"
	"sms_gateway/task"
	"sms_lib/utils"
)

type program struct {
}

func main() {
	flag.Parse()
	if *utils.Cpu {
		defer profile.Start().Stop()
	}
	if *utils.Mem {
		defer profile.Start(profile.MemProfile).Stop()
	}
	task.LoopSrvMain()
}
