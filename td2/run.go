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
	return runContext(context.Background(), configFile, stateFile, chainConfigDirectory, password)
}

func runContext(ctx context.Context, configFile, stateFile, chainConfigDirectory string, password *string) error {
	var err error
	td, err = loadConfig(configFile, stateFile, chainConfigDirectory, password)
	if err != nil {
		return err
	}
	defer td.cancel()
	defer td.deliveryCancel()
	fatal, problems := validateConfig(td)
	for _, problem := range problems {
		fmt.Println(problem)
	}
	if fatal {
		return errors.New("tenderduty configuration is invalid")
	}
	log.Println("tenderduty config is valid, starting with", len(td.Chains), "chains")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	failures := make(chan error, 2)
	td.startWorker(func() {
		select {
		case <-ctx.Done():
			td.cancel()
		case <-td.ctx.Done():
		}
	})
	td.restoreRecoveries()
	td.startWorker(td.runNotifications)
	if td.EnableDash {
		td.startWorker(func() {
			if err := dash.Serve(td.ctx, td.Listen, td.updateChan, td.logChan, td.HideLogs, td.healthStatus); err != nil {
				failures <- err
			}
		})
	} else {
		td.startWorker(func() {
			for {
				select {
				case <-td.updateChan:
				case <-td.ctx.Done():
					return
				}
			}
		})
	}
	if td.Prom {
		td.startWorker(func() {
			if err := prometheusExporter(td.ctx, td.statsChan); err != nil {
				failures <- err
			}
		})
	} else {
		td.startWorker(func() {
			for {
				select {
				case <-td.statsChan:
				case <-td.ctx.Done():
					return
				}
			}
		})
	}
	td.pingHealthcheck()
	for _, chain := range td.Chains {
		if chain.PublicFallback {
			td.startWorker(func() {
				for {
					if err := refreshRegistry(td.ctx); err != nil {
						l("could not refresh registry paths:", err)
					}
					if !waitContext(td.ctx, 12*time.Hour) {
						return
					}
				}
			})
			break
		}
	}
	for name, chain := range td.Chains {
		name, chain := name, chain
		td.startWorker(chain.watch)
		td.startWorker(func() { chain.monitorHealth(td.ctx, name) })
		td.startWorker(func() {
			for td.ctx.Err() == nil {
				if err := chain.newRpc(); err != nil {
					l(chain.ChainId, err)
					if !waitContext(td.ctx, 5*time.Second) {
						return
					}
					continue
				}
				if err := chain.GetValInfo(true); err != nil {
					l(chain.ChainId, err)
					info, _ := chain.validatorState()
					if len(info.Conspub) != 20 || info.Valcons == "" {
						if !waitContext(td.ctx, 5*time.Second) {
							return
						}
						continue
					}
				}
				chain.WsRun()
				if !waitContext(td.ctx, 5*time.Second) {
					return
				}
			}
		})
	}
	td.startWorker(func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := td.persistState(); err != nil {
					log.Println("could not save state:", err)
				}
			case <-td.ctx.Done():
				return
			}
		}
	})
	select {
	case <-signals:
	case err = <-failures:
	case <-td.ctx.Done():
	}
	td.cancel()
	stopped := make(chan struct{})
	go func() { td.workers.Wait(); close(stopped) }()
	// Allow accepted notification requests to finish before saving final delivery state.
	select {
	case <-stopped:
	case <-time.After(20 * time.Second):
		td.deliveryCancel()
		<-stopped
	}
	if saveErr := td.persistState(); saveErr != nil {
		return fmt.Errorf("could not save final state: %w", saveErr)
	}
	return err
}

func saveState(stateFile string) error {
	td.saveMux.Lock()
	defer td.saveMux.Unlock()
	td.chainsMux.RLock()
	blocks := make(map[string][]int)
	lastBlocks := make(map[string]time.Time)
	heights := make(map[string]int64)
	nodesDown := make(map[string]map[string]time.Time)
	for name, chain := range td.Chains {
		chain.stateMux.RLock()
		blocks[name] = append([]int(nil), chain.blocksResults...)
		lastBlocks[name] = chain.lastBlockTime
		heights[name] = chain.lastBlockNum
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
	data, err := json.Marshal(&savedState{Alarms: alarms, Blocks: blocks, LastBlocks: lastBlocks, NodesDown: nodesDown, Heights: heights})
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
