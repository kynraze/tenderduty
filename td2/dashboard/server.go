package dash

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"github.com/gorilla/websocket"
	"io/fs"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

var (
	Content embed.FS
	rootDir fs.FS
	rex     = regexp.MustCompile(`(?:https?|tcp|wss?)://[^\s]+`)
)

const (
	logLength = 256
	// websocket clients at once, the rest get a 503
	maxWebsocketClients = 256
)

// limitClients returns 503 when long-lived requests reach the limit.
func limitClients(limit int, next http.HandlerFunc) http.HandlerFunc {
	slots := make(chan struct{}, limit)
	return func(writer http.ResponseWriter, request *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next(writer, request)
		default:
			http.Error(writer, "too many clients", http.StatusServiceUnavailable)
		}
	}
}

func statusSnapshot(status *ChainStatus, hideLogs bool) *ChainStatus {
	copy := *status
	copy.Blocks = append([]int(nil), status.Blocks...)
	copy.RPCNodes = append([]RPCNodeStatus(nil), status.RPCNodes...)
	if hideLogs {
		copy.RPCNodes = nil
		copy.LastError = rex.ReplaceAllString(copy.LastError, "-redacted-")
		copy.Fibre = copy.Fibre.withoutDetails()
	}
	return &copy
}

func Serve(ctx context.Context, port string, updates chan *ChainStatus, logs chan LogMessage, hideLogs bool, health func() HealthStatus, fibre ...func(string, string) (FibreHistory, error)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var err error
	rootDir, err = fs.Sub(Content, "static")
	if err != nil {
		return err
	}
	var cast broadcaster

	// cache the json .... don't serialize on-demand
	logCache, statusCache := []byte("[]"), []byte(`{"msgType":"update","Status":[]}`)
	cacheMux := sync.RWMutex{}

	statusMux := sync.Mutex{}
	status := make(map[string]*ChainStatus)
	logSlice := make([]LogMessage, 0)

	type statusUpdate struct {
		MessageType string `json:"msgType"`
		Status      []*ChainStatus
	}

	finished := make(chan struct{})
	defer func() { cancel(); cast.close(); <-finished }()
	go func() {
		defer close(finished)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		update := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if update {
					cacheMux.RLock()
					cast.send(statusCache)
					cacheMux.RUnlock()
					update = false
				}

			case u := <-updates:
				if message := fibreNotification(u); message != nil {
					cast.send(message)
					continue
				}
				// try to catch any accidental rpc endpoint leaks
				u = statusSnapshot(u, hideLogs)
				statusMux.Lock() // probably unnecessary
				if u.MsgType == "remove" {
					delete(status, u.Name)
				} else {
					status[u.Name] = u
				}
				result := make([]*ChainStatus, 0)
				for k := range status {
					result = append(result, status[k])
				}
				statusMux.Unlock()
				sort.Slice(result, func(i, j int) bool {
					return result[i].Name < result[j].Name
				})
				j, e := json.Marshal(statusUpdate{
					MessageType: "update",
					Status:      result,
				})
				if e != nil {
					continue
				}
				cacheMux.Lock()
				statusCache = j
				cacheMux.Unlock()
				update = true

			case l := <-logs:
				if hideLogs {
					continue
				}
				if len(logSlice) >= logLength {
					logSlice = append([]LogMessage{l}, logSlice[0:len(logSlice)-1]...)
				} else {
					logSlice = append([]LogMessage{l}, logSlice...)
				}
				j, e := json.Marshal(logSlice)
				if e != nil {
					continue
				}
				cacheMux.Lock()
				logCache = j
				cacheMux.Unlock()
				j, e = json.Marshal(l)
				if e != nil {
					continue
				}
				cast.send(j)
			}
		}
	}()

	var upgrader = websocket.Upgrader{}
	upgrader.EnableCompression = true

	mux := http.NewServeMux()
	if len(fibre) > 0 {
		provider := fibre[0]
		if hideLogs {
			provider = hideFibreDetails(provider)
		}
		mux.HandleFunc("/fibre", FibreHandler(provider))
	}
	for _, path := range []string{"/health", "/ready"} {
		mux.HandleFunc(path, healthHandler(health, path == "/ready"))
	}
	mux.HandleFunc("/ws", limitClients(maxWebsocketClients, func(writer http.ResponseWriter, request *http.Request) {
		c, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer c.Close()
		sub, stop := cast.listen()
		defer stop()
		for message := range sub {
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			e := c.WriteMessage(websocket.TextMessage, message)
			if e != nil {
				return
			}
		}
	}))

	mux.HandleFunc("/logsenabled", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		j, _ := json.Marshal(map[string]bool{"enabled": !hideLogs})
		_, _ = writer.Write(j)
	})

	mux.HandleFunc("/logs", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		cacheMux.RLock()
		defer cacheMux.RUnlock()
		_, _ = writer.Write(logCache)
	})

	mux.HandleFunc("/state", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		cacheMux.RLock()
		defer cacheMux.RUnlock()
		_, _ = writer.Write(statusCache)
	})

	mux.Handle("/", &CacheHandler{})
	server := newServer(port, mux)
	serverDone := make(chan struct{})
	defer close(serverDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Close()
		case <-serverDone:
		}
	}()
	err = server.ListenAndServe()
	cast.close()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newServer(port string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 3 * time.Second,
		// a hijacked websocket is not bound by these
		ReadTimeout: 15 * time.Second,
		IdleTimeout: 60 * time.Second,
	}
}

func healthHandler(health func() HealthStatus, readiness bool) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		status := health()
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("Cache-Control", "no-store")
		if !status.Alive || (readiness && !status.Ready) {
			writer.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(writer).Encode(status)
	}
}

// CacheHandler implements the Handler interface with a Cache-Control set on responses
type CacheHandler struct{}

func (ch CacheHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	// revalidate scripts and markup on each request
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Powered-By", "https://github.com/kynraze/tenderduty")
	http.FileServer(http.FS(rootDir)).ServeHTTP(writer, request)
}
