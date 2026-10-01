package dash

import (
	"embed"
	"encoding/json"
	"github.com/gorilla/websocket"
	"github.com/textileio/go-threads/broadcast"
	"io/fs"
	"log"
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

const logLength = 256

func statusSnapshot(status *ChainStatus, hideLogs bool) *ChainStatus {
	copy := *status
	copy.Blocks = append([]int(nil), status.Blocks...)
	if hideLogs {
		copy.LastError = rex.ReplaceAllString(copy.LastError, "-redacted-")
	}
	return &copy
}

func Serve(port string, updates chan *ChainStatus, logs chan LogMessage, hideLogs bool) {
	var err error
	rootDir, err = fs.Sub(Content, "static")
	if err != nil {
		log.Fatalln(err)
	}
	var cast broadcast.Broadcaster

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

	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		update := false
		for {
			select {
			case <-tick.C:
				if update {
					cacheMux.RLock()
					_ = cast.Send(statusCache)
					cacheMux.RUnlock()
					update = false
				}

			case u := <-updates:
				// try to catch any accidental rpc endpoint leaks
				u = statusSnapshot(u, hideLogs)
				statusMux.Lock() // probably unnecessary
				status[u.Name] = u
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
				_ = cast.Send(j)
			}
		}
	}()

	var upgrader = websocket.Upgrader{}
	upgrader.EnableCompression = true

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(writer http.ResponseWriter, request *http.Request) {
		c, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer c.Close()
		sub := cast.Listen()
		defer sub.Discard()
		for message := range sub.Channel() {
			_ = c.SetWriteDeadline(time.Now().Add(10 * time.Second))
			e := c.WriteMessage(websocket.TextMessage, message.([]byte))
			if e != nil {
				return
			}
		}
	})

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
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}
	err = server.ListenAndServe()
	cast.Discard()
	log.Fatal("tenderduty dashboard server failed", err)
}

// CacheHandler implements the Handler interface with a Cache-Control set on responses
type CacheHandler struct{}

func (ch CacheHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "public, max-age=3600")
	writer.Header().Set("X-Powered-By", "https://github.com/blockpane/tenderduty")
	http.FileServer(http.FS(rootDir)).ServeHTTP(writer, request)
}
