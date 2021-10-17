package task

import (
	"encoding/json"
	"os"
	"os/signal"
	"sms_lib/config"
	"sms_lib/models"
	"sms_lib/utils"
	"strconv"
	"syscall"
)

func signalHandle(cs *ChannelStat) {

	exitChan := make(chan os.Signal)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGKILL)
	s := <-exitChan
	logger.Debug().Msgf("接收到退出信号：%v，关闭所有通道", s)
	rKey := config.GetRedis().Key
	str := models.RedisGet(rKey)
	accounts := &models.Accounts{}
	err := json.Unmarshal([]byte(str), &accounts.Channel)
	if err != nil {
		logger.Error().Msgf("accounts json.unmarshal error:%v, exit...", err)
		return
	}
	//for _, channel := range
	for chid, conns := range cs.conns {
		if conns > 0 {
			for i := 1; i <= conns; i++ {
				runId := strconv.Itoa(chid) + ":" + strconv.Itoa(i)
				select {
				case utils.ExitSig.LoopSend[runId] <- true:
				default:
					logger.Debug().Msgf("通道(%d) 通知关闭失败", chid)
				}
			}
		}
	}
	//atomic.StoreInt32(&signalExit, 1)
	close(signalExit)
}
