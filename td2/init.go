package tenderduty

import (
	"embed"
	"fmt"
	dash "github.com/kynraze/tenderduty/v2/td2/dashboard"
	"log"
	"os"
	"strings"
	"time"
)

//go:embed static/*
var content embed.FS

func init() {
	log.SetFlags(log.LstdFlags)
	log.SetOutput(os.Stderr)
	dash.Content = content

	// use a channel for logging, two reasons: several logs could hit at once (formatting,) and to broadcast
	// messages to the monitoring dashboard
	go func() {
		for msg := range logs {
			msg = strings.TrimRight(strings.TrimLeft(fmt.Sprint(msg), "["), "]")
			log.Println("tenderduty | ", msg)
		}
	}()
}

var logs = make(chan interface{}, 128)

func l(v ...any) {
	c := td
	if c.EnableDash && !c.HideLogs && c.logChan != nil {
		select {
		case c.logChan <- dash.LogMessage{MsgType: "log", Ts: time.Now().UTC().Unix(), Msg: strings.TrimSpace(fmt.Sprintln(v...))}:
		default:
		}
	}
	select {
	case logs <- v:
	default:
		log.Println(v...)
	}
}
