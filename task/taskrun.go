package task

import (
	"os"
	"runtime"
	"sms_gateway/server"
	"sms_lib/config"
	"sms_lib/levellogger"
	"sms_lib/models"
	"sms_lib/utils"
	"time"
)

//var mLock = sync.Mutex{}
var signalExit chan struct{}

type ChannelStat struct {
	conns map[int]int
	//wg          *sync.WaitGroup
	waitGroup   utils.WaitGroupWrapper
	accountsKey string
	testId      int
	exclusiveId int //0:共享通道，
}

func ServerSupervisory() {
	logger.Debug().Msgf("启动 ServerSupervisory 协程...")
	//var err error
	timeout := time.Duration(30) * time.Second
	timer := time.NewTimer(timeout)
	threshold := config.GetThreshold()
	for {
		utils.ResetTimer(timer, timeout)

		logger.Warn().Msgf("当前运行中的协程数量：%d", runtime.NumGoroutine())

		utils.PrintMemStats()

		if utils.GetCpuPercent() > threshold {
			logger.Warn().Msgf("当前节点cpu使用率已超%2.f%%,负载过高", threshold)
		}
		select {
		case <-server.SignalExit:
			logger.Debug().Msgf("退出 ServerSupervisory 协程...")
			return
		case <-timer.C:
		}
	}
}

func LoopSrvMain() {
	//var wg sync.WaitGroup
	var err error
	utils.ErrInit()
	models.InitDB()
	models.InitRedis()
	server.InitNodeId()
	levellogger.Llogger = levellogger.NewLogger("")
	server.NewSnowflakeNode()
	server.SeqId = server.InitSeqId()

	var topics []string
	//cfg := config.GetTopicPrefix()
	//NewProducer topic 初始化
	models.Prn, err = models.NewTopicPubMgr(topics)
	if err != nil {
		logger.Error().Msgf("Producer NewProducer error:%v", err)
		return
	}
	go ServerSupervisory()

	server.Listen()

	logger.Debug().Msgf("退出网关主程序")
	os.Exit(0)
}
