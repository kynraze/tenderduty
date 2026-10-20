package tenderduty

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type rawCommit struct {
	Result struct {
		SignedHeader struct {
			Header struct {
				ProposerAddress string `json:"proposer_address"`
			} `json:"header"`
			Commit struct {
				Height     stringInt64 `json:"height"`
				Signatures []signature `json:"signatures"`
			} `json:"commit"`
		} `json:"signed_header"`
	} `json:"result"`
}

// commitStatus returns the validator's status at a height; prevote/precommit states only exist live.
func commitStatus(ctx context.Context, client *http.Client, remote, address string, height int64) (StatusType, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/commit?height=%d", strings.TrimRight(remote, "/"), height), nil)
	if err != nil {
		return -1, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	commit := rawCommit{}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&commit); err != nil {
		return -1, err
	}
	header := commit.Result.SignedHeader
	if header.Commit.Height.val() != height {
		return -1, fmt.Errorf("no commit for height %d", height)
	}
	block := rawBlock{}
	block.Block.LastCommit.Signatures = header.Commit.Signatures
	switch {
	case !block.find(address):
		return Statusmissed, nil
	case header.Header.ProposerAddress == address:
		return StatusProposed, nil
	default:
		return StatusSigned, nil
	}
}

// backfillBlocks matches the length of the dashboard's block trace.
const backfillBlocks = 160

func (cc *ChainConfig) emptyHeights() []int64 {
	cc.stateMux.RLock()
	defer cc.stateMux.RUnlock()
	heights := make([]int64, 0)
	for index, status := range cc.blocksResults {
		if index >= backfillBlocks {
			break
		}
		if height := cc.lastBlockNum - 1 - int64(index); status == -1 && height > 0 {
			heights = append(heights, height)
		}
	}
	return heights
}

// backfill fills empty block history from commits; counters and alarms stay live.
func (cc *ChainConfig) backfill(ctx context.Context, remote string, wanted []int64) {
	info, _ := cc.validatorState()
	if !info.Bonded || len(info.Conspub) == 0 || len(wanted) == 0 {
		return
	}
	address := strings.ToUpper(hex.EncodeToString(info.Conspub))
	client := &http.Client{Timeout: 10 * time.Second}
	heights := make(chan int64)
	var workers sync.WaitGroup
	var mux sync.Mutex
	filled, missed := 0, 0
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for height := range heights {
				status, err := commitStatus(ctx, client, remote, address, height)
				if err != nil || !cc.fillHeight(height, status) {
					continue
				}
				mux.Lock()
				filled++
				if status == Statusmissed {
					missed++
				}
				mux.Unlock()
			}
		}()
	}
	for _, height := range wanted {
		select {
		case heights <- height:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(heights)
	workers.Wait()
	if filled == 0 {
		return
	}
	l(fmt.Sprintf("filled %d of %d blocks that were not observed on %s, %d missed", filled, len(wanted), cc.ChainId, missed))
	if td.EnableDash {
		td.sendUpdate(cc.dashboardStatus())
	}
}

// fillHeight maps lastBlockNum-1 to the newest slot; commits sign the previous height.
func (cc *ChainConfig) fillHeight(height int64, status StatusType) bool {
	cc.stateMux.Lock()
	defer cc.stateMux.Unlock()
	index := cc.lastBlockNum - 1 - height
	if index < 0 || index >= int64(len(cc.blocksResults)) || cc.blocksResults[index] != -1 {
		return false
	}
	cc.blocksResults[index] = int(status)
	return true
}
