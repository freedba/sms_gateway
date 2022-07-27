package task

import (
	"encoding/json"
	"os"
	"runtime"
	"sms_gateway/server"
	"sms_lib/config"
	"sms_lib/levellogger"
	"sms_lib/models"
	"sms_lib/utils"
	"time"
)

//var signalExit chan struct{}

func ServerSupervise(sess *server.Sessions) {
	logger.Debug().Msgf("启动 ServerSupervisory 协程...")
	timeout := utils.Timeout * 6
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	threshold := config.GetThreshold()
	rKey := "index:user:userinfo:"
	account := server.AccountsInfo{}

	for {
		utils.ResetTimer(timer, timeout)

		logger.Warn().Msgf("当前运行中的协程数量：%d", runtime.NumGoroutine())

		utils.PrintMemStats()

		server.EtcdCli.LeaseTTL()

		if utils.GetCpuPercent() > threshold {
			logger.Warn().Msgf("当前节点cpu使用率已超%2.f%%,负载过高", threshold)
		}

		for user, conn := range sess.Users {
			logger.Info().Msgf("账号(%s) 已建立的连接数：%d", user, len(conn))
			str := models.RedisHGet(rKey, user)
			if str == "" {
				logger.Debug().Msgf("账号(%s) 不存在，关闭账号连接", user)
				sess.Close(user)
			} else {
				_ = json.Unmarshal([]byte(str), &account)
				logger.Debug().Msgf("account.FlowVelocity:%d", account.FlowVelocity)
				if account.FlowVelocity == 0 {
					logger.Debug().Msgf("账号(%s) 流控值为0，关闭账号连接", user)
					sess.Close(user)
				}
			}
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
	var err error
	models.InitDB()
	models.InitRedis()
	server.InitNodeId()
	server.FakeGateway = config.GetFakeGateway()
	if server.FakeGateway == 1 {
		logger.Info().Msgf("当前运行模拟网关模式")
	}
	levellogger.LLogger = levellogger.NewLogger("")
	server.NewSnowflakeNode()
	server.SeqId = server.InitSeqId()
	// init etcd
	server.NewEtcd()
	server.EtcdCli.LeaseGrant(30)
	go server.EtcdCli.LeaseRenew()

	debug := utils.GetEnv("DEBUG")
	if debug != "" {
		if debug == "1" {
			utils.Debug = true
		} else {
			utils.Debug = false
		}
	} else {
		utils.Debug = config.GetDebug()
	}

	var topics []string
	models.Prn, err = models.NewTopicPubMgr(topics)
	if err != nil {
		logger.Error().Msgf("Producer NewProducer error:%v", err)
		return
	}

	sess := &server.Sessions{
		Users: make(map[string][]*server.SrvConn),
	}

	go ServerSupervise(sess)

	server.Listen(sess)

	logger.Debug().Msgf("退出网关主程序")
	os.Exit(0)
}
