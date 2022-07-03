package server

import (
	"fmt"
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
	for user, srvList := range sess.Users {
		for i := 0; i < len(srvList); i++ {
			s := sess.Users[user][i]
			runId := user + ":" + fmt.Sprintf("%p", s.conn)
			logger.Debug().Msgf("账号(%s) 关闭连接,close(s.ExitSrv)", runId)
			if !utils.ChIsClosed(s.ExitSrv) {
				close(s.ExitSrv)
			}
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
