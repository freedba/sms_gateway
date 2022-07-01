package server

import (
	"os"
	"os/signal"
	"sms_lib/utils"
	"syscall"
	"time"
)

func signalHandle(sess *Sessions) {

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGKILL)
	sig := <-exitChan
	logger.Debug().Msgf("接收到退出信号：%v，关闭所有账号连接", sig)
	for name, strPoints := range sess.Users {
		for i := 0; i < len(strPoints); i++ {
			runId := name + ":" + sess.Users[name][i]
			logger.Debug().Msgf("关闭账号:%s", runId)
			close(utils.ExitSig.LoopRead[runId])
		}
	}

	for i := 0; i < 5; i++ {
		tLen := len(sess.Users)
		if tLen == 0 {
			break
		}
		logger.Debug().Msgf("check sess.Users num: %d, try nums:%d", tLen, i)
		time.Sleep(time.Duration(1) * time.Second)
		i++
	}

	close(SignalExit)

	if err := sess.ln.Close(); err != nil {
		logger.Error().Msgf("sess.ln.Close error: %v", err)
	}

	logger.Debug().Msgf("Exiting signalHandle...")
}
