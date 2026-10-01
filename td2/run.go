package tenderduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	dash "github.com/blockpane/tenderduty/v2/td2/dashboard"
)

var td = &Config{}

func Run(configFile, stateFile, chainConfigDirectory string, password *string) error {
	var err error
	td, err = loadConfig(configFile, stateFile, chainConfigDirectory, password)
	if err != nil {
		return err
	}
	fatal, problems := validateConfig(td)
	for _, p := range problems {
		fmt.Println(p)
	}
	if fatal {
		log.Fatal("tenderduty the configuration is invalid, refusing to start")
	}
	log.Println("tenderduty config is valid, starting tenderduty with", len(td.Chains), "chains")

	defer td.cancel()

	go func() {
		for {
			select {
			case alert := <-td.alertChan:
				if alert.pd {
					go deliverWithRetry(td.ctx, alert, pd, "pagerduty", notifyPagerduty)
				}
				if alert.disc {
					go deliverWithRetry(td.ctx, alert, di, "discord", notifyDiscord)
				}
				if alert.tg {
					go deliverWithRetry(td.ctx, alert, tg, "telegram", notifyTg)
				}
				if alert.slk {
					go deliverWithRetry(td.ctx, alert, slk, "slack", notifySlack)
				}
			case <-td.ctx.Done():
				return
			}
		}
	}()
	alarms.notifyMux.RLock()
	recoveries := make([]pendingRecovery, 0, len(alarms.PendingRecoveries))
	for _, recovery := range alarms.PendingRecoveries {
		if recovery != nil {
			recoveries = append(recoveries, *recovery)
		}
	}
	alarms.notifyMux.RUnlock()
	for _, recovery := range recoveries {
		if td.Chains[recovery.Chain] != nil {
			td.alert(recovery.Chain, recovery.Message, recovery.Severity, true, &recovery.ID)
		}
	}

	if td.EnableDash {
		go dash.Serve(td.Listen, td.updateChan, td.logChan, td.HideLogs)
		l("starting dashboard on", td.Listen)
	} else {
		go func() {
			for {
				<-td.updateChan
			}
		}()
	}
	if td.Prom {
		go prometheusExporter(td.ctx, td.statsChan)
	} else {
		go func() {
			for {
				<-td.statsChan
			}
		}()
	}

	// tenderduty health checks:
	if td.Healthcheck.Enabled {
		td.pingHealthcheck()
	}

	for k := range td.Chains {
		cc := td.Chains[k]

		go func(cc *ChainConfig, name string) {
			// alert worker
			go cc.watch()

			// node health checks:
			go cc.monitorHealth(td.ctx, name)

			// websocket subscription and occasional validator info refreshes
			for {
				select {
				case <-td.ctx.Done():
					return
				default:
				}
				e := cc.newRpc()
				if e != nil {
					l(cc.ChainId, e)
					time.Sleep(5 * time.Second)
					continue
				}
				e = cc.GetValInfo(true)
				if e != nil {
					l("🛑", cc.ChainId, e)
					info, _ := cc.validatorState()
					if len(info.Conspub) == 0 || info.Valcons == "" {
						time.Sleep(5 * time.Second)
						continue
					}
				}
				cc.WsRun()
				l(cc.ChainId, "🌀 websocket exited! Restarting monitoring")
				time.Sleep(5 * time.Second)
			}
		}(cc, k)
	}

	// save state periodically and on exit
	saved := make(chan interface{})
	go saveOnExit(stateFile, saved)

	<-td.ctx.Done()
	<-saved

	return err
}

func deliverWithRetry(ctx context.Context, msg *alertMsg, dest notifyDest, service string, send func(*alertMsg) error) {
	delay := 5 * time.Second
	for attempt := 0; ; attempt++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		if attempt > 0 && !msg.resolved && !alarmActive(msg) {
			return
		}
		err := send(msg)
		if err == nil {
			return
		}
		if !errors.Is(err, errNotificationBusy) {
			l(msg.chain, "error sending alert to", service, err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 5*time.Minute {
			delay *= 2
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		}
	}
}

func saveOnExit(stateFile string, saved chan interface{}) {
	quitting := make(chan os.Signal, 1)
	signal.Notify(quitting, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(quitting)
	defer close(saved)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := saveState(stateFile); err != nil {
				log.Println("could not save state:", err)
			}
		case <-td.ctx.Done():
			if err := saveState(stateFile); err != nil {
				log.Println("could not save state:", err)
			}
			return
		case <-quitting:
			td.cancel()
			if err := saveState(stateFile); err != nil {
				log.Println("could not save state:", err)
			}
			return
		}
	}
}

func saveState(stateFile string) error {
	td.chainsMux.RLock()
	blocks := make(map[string][]int)
	lastBlocks := make(map[string]time.Time)
	nodesDown := make(map[string]map[string]time.Time)
	for name, chain := range td.Chains {
		chain.stateMux.RLock()
		blocks[name] = append([]int(nil), chain.blocksResults...)
		lastBlocks[name] = chain.lastBlockTime
		chain.stateMux.RUnlock()
		for _, node := range chain.Nodes {
			status := node.snapshot()
			if status.down {
				if nodesDown[name] == nil {
					nodesDown[name] = make(map[string]time.Time)
				}
				nodesDown[name][node.Url] = status.downSince
			}
		}
	}
	td.chainsMux.RUnlock()

	alarms.notifyMux.RLock()
	data, err := json.Marshal(&savedState{Alarms: alarms, Blocks: blocks, LastBlocks: lastBlocks, NodesDown: nodesDown})
	alarms.notifyMux.RUnlock()
	if err != nil {
		return err
	}
	// Write the new state before replacing the old file.
	file, err := os.CreateTemp(filepath.Dir(stateFile), ".tenderduty-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), stateFile)
}
