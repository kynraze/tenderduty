package tenderduty

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	pbtypes "github.com/tendermint/tendermint/proto/tendermint/types"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	QueryNewBlock string = `tm.event='NewBlock'`
	QueryVote     string = `tm.event='Vote'`
)

// StatusType represents the various possible end states. Prevote and Precommit are special cases, where the node
// monitoring for misses did see them, but the proposer did not include in the block.
type StatusType int

const (
	Statusmissed StatusType = iota
	StatusPrevote
	StatusPrecommit
	StatusSigned
	StatusProposed
)

// StatusUpdate is passed over a channel from the websocket client indicating the current state, it is immediate in the
// case of prevotes etc, and the highest value seen is used in the final determination (which is how we tag
// prevote/precommit + missed blocks.
type StatusUpdate struct {
	Height     int64
	Status     StatusType
	Final      bool
	HeadHeight int64
}

// WsReply is a trimmed down version of the JSON sent from a tendermint websocket subscription.
type WsReply struct {
	Id    int64 `json:"id"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Result struct {
		Query string `json:"query"`
		Data  struct {
			Type  string          `json:"type"`
			Value json.RawMessage `json:"value"`
		} `json:"data"`
	} `json:"result"`
}

// Type is the abci message type
func (wsr WsReply) Type() string {
	return wsr.Result.Data.Type
}

// Value returns the JSON encoded raw bytes from the response. Unlike an ABCI RPC query, these are not protobuf.
func (wsr WsReply) Value() []byte {
	if wsr.Result.Data.Value == nil {
		return make([]byte, 0)
	}
	return wsr.Result.Data.Value
}

// WsRun is our main entrypoint for the websocket listener. In the Run loop it will block, and if it exits force a
// renegotiation for a new client.
func (cc *ChainConfig) WsRun() {
	ctx, cancel := context.WithCancel(td.context())
	client := cc.clientSnapshot()
	info, _ := cc.validatorState()
	if client == nil || len(info.Conspub) != 20 {
		cancel()
		return
	}
	conn, err := newClientContext(ctx, client.Remote(), true)
	if err != nil {
		cancel()
		l(cc.ChainId, err)
		return
	}
	var workers sync.WaitGroup
	defer func() { cancel(); _ = conn.Close(); workers.Wait(); cc.setMonitoring(false) }()
	conn.SetReadLimit(16 << 20)
	_ = conn.SetCompressionLevel(3)
	results := make(chan StatusUpdate)
	votes, blocks := make(chan *WsReply), make(chan *WsReply)
	address := strings.ToUpper(hex.EncodeToString(info.Conspub))
	start := func(work func()) {
		workers.Add(1)
		go func() { defer workers.Done(); work() }()
	}
	start(func() { cc.processResults(ctx, results) })
	start(func() { handleVotes(ctx, votes, results, address) })
	start(func() {
		if err := handleBlocks(ctx, blocks, results, address); err != nil {
			l(cc.ChainId, err)
		}
		cancel()
	})
	start(func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				cancel()
				return
			}
			reply := &WsReply{}
			if json.Unmarshal(data, reply) != nil {
				continue
			}
			if reply.Error != nil {
				l(cc.ChainId, "websocket subscription rejected:", reply.Error.Message)
				cancel()
				return
			}
			var target chan *WsReply
			switch reply.Type() {
			case "tendermint/event/NewBlock":
				target = blocks
			case "tendermint/event/Vote":
				target = votes
			default:
				continue
			}
			select {
			case target <- reply:
			case <-ctx.Done():
				return
			}
		}
	})
	for id, query := range []string{QueryNewBlock, QueryVote} {
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		request := fmt.Sprintf(`{"jsonrpc":"2.0","method":"subscribe","id":%d,"params":{"query":"%s"}}`, id+1, query)
		if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
			cancel()
			return
		}
	}
	l(cc.ChainId, "watching for NewBlock and Vote events via", client.Remote())
	<-ctx.Done()
}

type signingTracker struct {
	votes     map[int64]StatusType
	finalized int64
}

func (tracker *signingTracker) consume(update StatusUpdate) (StatusType, bool) {
	if update.Height <= tracker.finalized {
		return -1, false
	}
	if tracker.votes == nil {
		tracker.votes = make(map[int64]StatusType)
	}
	if !update.Final {
		// Bound vote history when an endpoint sends votes without finalized blocks.
		if len(tracker.votes) < 64 || tracker.votes[update.Height] != 0 {
			if update.Status > tracker.votes[update.Height] {
				tracker.votes[update.Height] = update.Status
			}
		}
		return -1, false
	}
	status, vote := update.Status, tracker.votes[update.Height]
	if status == StatusSigned && vote == StatusProposed {
		status = StatusProposed
	}
	if status == Statusmissed && vote <= StatusPrecommit && vote > status {
		status = vote
	}
	tracker.finalized = update.Height
	for height := range tracker.votes {
		if height <= tracker.finalized {
			delete(tracker.votes, height)
		}
	}
	return status, true
}

func (cc *ChainConfig) processResults(ctx context.Context, results <-chan StatusUpdate) {
	cc.stateMux.RLock()
	tracker := signingTracker{finalized: cc.lastBlockNum - 1}
	cc.stateMux.RUnlock()
	for {
		select {
		case <-ctx.Done():
			return
		case update := <-results:
			status, final := tracker.consume(update)
			if !final {
				continue
			}
			info, _ := cc.validatorState()
			if !info.Bonded {
				status = -1
			}
			cc.stateMux.Lock()
			previousTime, previousHeight := cc.lastBlockTime, cc.lastBlockNum
			gap := update.HeadHeight - previousHeight - 1
			if previousHeight > 0 && gap > 0 {
				cc.statConsecutiveMiss = 0
				if gap > int64(len(cc.blocksResults)) {
					gap = int64(len(cc.blocksResults))
				}
				for i := int64(0); i < gap; i++ {
					cc.blocksResults = append([]int{-1}, cc.blocksResults[:len(cc.blocksResults)-1]...)
				}
			}
			cc.lastBlockNum, cc.lastBlockTime = update.HeadHeight, time.Now()
			cc.observedBlock = true
			cc.monitoring, cc.monitoringSince = true, time.Now()
			cc.blocksResults = append([]int{int(status)}, cc.blocksResults[:len(cc.blocksResults)-1]...)
			switch status {
			case Statusmissed, StatusPrevote, StatusPrecommit:
				cc.statTotalMiss++
				cc.statConsecutiveMiss++
				if status == StatusPrevote {
					cc.statPrevoteMiss++
				}
				if status == StatusPrecommit {
					cc.statPrecommitMiss++
				}
			case StatusSigned, StatusProposed:
				cc.statTotalSigns++
				cc.statConsecutiveMiss = 0
				if status == StatusProposed {
					cc.statTotalProps++
				}
			default:
				cc.statConsecutiveMiss = 0
			}
			counters := map[metricType]float64{metricSigned: cc.statTotalSigns, metricProposed: cc.statTotalProps, metricMissed: cc.statTotalMiss, metricPrevote: cc.statPrevoteMiss, metricPrecommit: cc.statPrecommitMiss, metricConsecutive: cc.statConsecutiveMiss}
			cc.stateMux.Unlock()
			if status >= 0 && status < StatusSigned {
				l(info.Moniker, "missed block", update.Height, "on", cc.ChainId)
			}
			if td.EnableDash {
				td.sendUpdate(cc.dashboardStatus())
			}
			if td.Prom {
				for metric, value := range counters {
					td.sendStat(cc.mkUpdate(metric, value, ""))
				}
				if !previousTime.IsZero() {
					td.sendStat(cc.mkUpdate(metricLastBlockSeconds, time.Since(previousTime).Seconds(), ""))
				}
			}
		}
	}
}

type stringInt64 string

// helper to make the "everything is a string" issue less painful.
func (si stringInt64) val() int64 {
	i, _ := strconv.ParseInt(string(si), 10, 64)
	return i
}

type signature struct {
	ValidatorAddress string `json:"validator_address"`
	BlockIDFlag      int    `json:"block_id_flag"`
}

// rawBlock is a trimmed down version of the block subscription result, it contains only what we need.
type rawBlock struct {
	Block struct {
		Header struct {
			Height          stringInt64 `json:"height"`
			ProposerAddress string      `json:"proposer_address"`
		} `json:"header"`
		LastCommit struct {
			Height     stringInt64 `json:"height"`
			Signatures []signature `json:"signatures"`
		} `json:"last_commit"`
	} `json:"block"`
}

// find determines if a validator's pre-commit was included in a finalized block.
func (rb rawBlock) find(val string) bool {
	if rb.Block.LastCommit.Signatures == nil {
		return false
	}
	for _, v := range rb.Block.LastCommit.Signatures {
		if v.ValidatorAddress == val && v.BlockIDFlag == 2 {
			return true
		}
	}
	return false
}

// handleBlocks consumes the channel for new blocks and when it sees one sends a status update. It's also
// responsible for stalled chain detection and will shutdown the client if there are no blocks for a minute.
func handleBlocks(ctx context.Context, blocks chan *WsReply, results chan StatusUpdate, address string) error {
	live := time.NewTicker(time.Minute)
	defer live.Stop()
	lastBlock := time.Now()
	for {
		select {
		case <-live.C:
			// no block for a full minute likely means we have either a dead chain, or a dead client.
			if lastBlock.Before(time.Now().Add(-time.Minute)) {
				return errors.New("websocket idle for 1 minute, exiting")
			}
		case block := <-blocks:
			b := &rawBlock{}
			err := json.Unmarshal(block.Value(), b)
			if err != nil {
				l("could not decode block", err)
				continue
			}
			upd := StatusUpdate{
				Height:     b.Block.LastCommit.Height.val(),
				HeadHeight: b.Block.Header.Height.val(),
				Status:     Statusmissed,
				Final:      true,
			}
			if upd.Height <= 0 || upd.HeadHeight != upd.Height+1 {
				continue
			}
			lastBlock = time.Now()
			if b.Block.Header.ProposerAddress == address {
				select {
				case results <- StatusUpdate{Height: upd.HeadHeight, Status: StatusProposed}:
				case <-ctx.Done():
					return nil
				}
			}
			if b.find(address) {
				upd.Status = StatusSigned
			}
			select {
			case results <- upd:
			case <-ctx.Done():
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// rawVote is a trimmed down version of the vote response.
type rawVote struct {
	Vote struct {
		Type             pbtypes.SignedMsgType `json:"type"`
		Height           stringInt64           `json:"height"`
		ValidatorAddress string                `json:"validator_address"`
	} `json:"Vote"`
}

// handleVotes consumes the channel for precommits and prevotes, tracking where in the process a validator is.
func handleVotes(ctx context.Context, votes chan *WsReply, results chan StatusUpdate, address string) {
	for {
		select {
		case reply := <-votes:
			vote := &rawVote{}
			err := json.Unmarshal(reply.Value(), vote)
			if err != nil {
				l(err)
				continue
			}
			if vote.Vote.ValidatorAddress == address {
				upd := StatusUpdate{Height: vote.Vote.Height.val()}
				switch vote.Vote.Type.String() {
				case "":
					continue
				case "SIGNED_MSG_TYPE_PREVOTE":
					upd.Status = StatusPrevote
				case "SIGNED_MSG_TYPE_PRECOMMIT":
					upd.Status = StatusPrecommit
				default:
					continue
				}
				if upd.Height <= 0 {
					continue
				}
				select {
				case results <- upd:
				case <-ctx.Done():
					return
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// TmConn is the websocket client. This is probably not necessary since I expected more complexity.
type TmConn struct {
	*websocket.Conn
}

// NewClient returns a websocket client.
// FIXME: need to handle UDS and insecure TLS
func NewClient(u string, allowInsecure bool) (*TmConn, error) {
	return newClientContext(context.Background(), u, allowInsecure)
}

func newClientContext(ctx context.Context, u string, allowInsecure bool) (*TmConn, error) {
	// dialUnix is used to determine if the connection is to a UDS and requires a custom dialer.
	var dialUnix bool

	// normalize the path, some public rpcs prefix with /rpc or similar.
	u = strings.TrimRight(u, "/")
	if !strings.HasSuffix(u, "/websocket") {
		u += "/websocket"
	}

	endpoint, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("parsing url in NewWsClient %s: %s", u, err.Error())
	}

	// normalize scheme to ws or wss
	switch endpoint.Scheme {
	case "http", "tcp", "ws":
		endpoint.Scheme = "ws"
	case "unix":
		dialUnix = true
		endpoint.Scheme = "ws"
	case "https", "wss":
		endpoint.Scheme = "wss"
	default:
		return nil, fmt.Errorf("protocol %s is unknown, valid choices are http, https, tcp, unix, ws, and wss", endpoint.Scheme)
	}

	// allowInsecure is primarily intended for self-signed certs, but it doesn't make sense to allow yes to for non-tls
	if endpoint.Scheme == "ws" && !allowInsecure {
		return nil, errors.New("allowInsecure must be true if protocol is not using TLS")
	}

	conn := &websocket.Conn{}

	switch {

	// TODO: add custom UDS dialer
	case dialUnix:
		return nil, errors.New("unix websocket endpoints are unsupported")

	// TODO: add custom TLS dialer to allow self-signed certs.
	// case allowInsecure && endpoint.Scheme == "wss":

	default:
		dialer := *websocket.DefaultDialer
		dialer.HandshakeTimeout = 10 * time.Second
		connected, response, dialErr := dialer.DialContext(ctx, endpoint.String(), nil)
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		conn, err = connected, dialErr
		if err != nil {
			return nil, fmt.Errorf("could not dial ws client to %s: %s", endpoint.String(), err.Error())
		}
	}
	return &TmConn{Conn: conn}, nil
}
