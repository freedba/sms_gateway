package server

import (
	"os"
	"os/signal"
	"syscall"
	"time"
)

func signalHandle(sess *Sessions) {

	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT, syscall.SIGHUP, syscall.SIGKILL)
	sig := <-exitChan
	logger.Debug().Msgf("接收到退出信号：%v，关闭所有账号连接", sig)
	for s := range sess.Conns {
		logger.Debug().Msgf("关闭账号:%s", s.RunId)
		close(s.exitSignalChan)
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

	err := sess.ln.Close()
	if err != nil {
		logger.Error().Msgf("sess.ln.Close error: %v", err)
	}

	logger.Debug().Msgf("Exiting signalHandle...")
}
