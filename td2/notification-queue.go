package tenderduty

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// queuedNotification contains no credentials. Destinations are loaded from config on restart.
type queuedNotification struct {
	Sequence    uint64     `json:"sequence"`
	Destination notifyDest `json:"destination"`
	pendingRecovery
	Resolved bool `json:"resolved"`
	// Queued starts the node-down grouping wait
	Queued   time.Time `json:"queued,omitempty"`
	snapshot *alertMsg // credentials kept only in memory, for removed chains
}

func (item queuedNotification) workerKey() string {
	return fmt.Sprintf("%d\x00%s\x00%s", item.Destination, item.Chain, item.Message)
}

// enqueue is called while notifyMux is held. Preserve incident transitions in order.
func (a *alarmCache) enqueue(msg *alertMsg) {
	for destination, enabled := range map[notifyDest]bool{pd: msg.pd, tg: msg.tg, di: msg.disc, slk: msg.slk} {
		if !enabled {
			continue
		}
		item := queuedNotification{Destination: destination, pendingRecovery: pendingRecovery{Chain: msg.chain, Message: msg.message, Severity: msg.severity, ID: msg.uniqueId, Lasted: msg.lasted, Quiet: msg.quiet}, Resolved: msg.resolved, Queued: time.Now(), snapshot: msg}
		var last *queuedNotification
		for i := range a.Outbox {
			if a.Outbox[i].workerKey() == item.workerKey() {
				last = &a.Outbox[i]
			}
		}
		if last != nil && last.Resolved == item.Resolved {
			continue
		}
		if last == nil && a.sentTo(destination, msg.chain+"\x00"+msg.message, msg.message) != msg.resolved {
			continue
		}
		a.NextNotification++
		item.Sequence = a.NextNotification
		a.Outbox = append(a.Outbox, item)
	}
}

// sentTo is called while notifyMux is held.
func (a *alarmCache) sentTo(destination notifyDest, key, legacy string) bool {
	var sent map[string]time.Time
	switch destination {
	case pd:
		sent = a.SentPdAlarms
	case tg:
		sent = a.SentTgAlarms
	case di:
		sent = a.SentDiAlarms
	case slk:
		sent = a.SentSlkAlarms
	}
	return !sent[key].IsZero() || !sent[legacy].IsZero()
}

func (c *Config) persistState() error {
	if c.stateFile == "" {
		return nil
	}
	return saveState(c.stateFile)
}

func (c *Config) restoreRecoveries() {
	alarms.notifyMux.RLock()
	recoveries := make([]pendingRecovery, 0, len(alarms.PendingRecoveries))
	for _, recovery := range alarms.PendingRecoveries {
		if recovery != nil {
			recoveries = append(recoveries, *recovery)
		}
	}
	alarms.notifyMux.RUnlock()
	for _, recovery := range recoveries {
		msg := c.makeAlert(recovery.Chain, recovery.Message, recovery.Severity, true, &recovery.ID)
		if msg == nil {
			continue
		}
		msg.lasted, msg.quiet = recovery.Lasted, recovery.Quiet
		alarms.notifyMux.Lock()
		for _, item := range alarms.Outbox {
			if item.Chain != recovery.Chain || item.Message != recovery.Message {
				continue
			}
			switch item.Destination {
			case pd:
				msg.pd = false
			case tg:
				msg.tg = false
			case di:
				msg.disc = false
			case slk:
				msg.slk = false
			}
		}
		alarms.enqueue(msg)
		alarms.notifyMux.Unlock()
	}
}

func (c *Config) runNotifications() {
	active := make(map[string]bool)
	c.chainsMux.RLock()
	done := make(chan string, len(c.Chains)*4+1)
	c.chainsMux.RUnlock()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if c.context().Err() != nil {
			return
		}
		alarms.notifyMux.RLock()
		keys := make(map[string]bool)
		for _, item := range alarms.Outbox {
			keys[item.workerKey()] = true
		}
		alarms.notifyMux.RUnlock()
		for key := range keys {
			if active[key] {
				continue
			}
			active[key] = true
			key := key
			c.startWorker(func() {
				c.deliverQueue(key)
				select {
				case done <- key:
				case <-c.context().Done():
				}
			})
		}
		select {
		case <-c.context().Done():
			return
		case key := <-done:
			delete(active, key)
		case <-c.alertChan:
		case <-ticker.C:
		}
	}
}

func (c *Config) queuedHead(key string) (queuedNotification, bool, bool) {
	alarms.notifyMux.RLock()
	defer alarms.notifyMux.RUnlock()
	return alarms.headOf(key)
}

// headOf skips unsent recoveries and returns the next notification. notifyMux held.
func (a *alarmCache) headOf(key string) (queuedNotification, bool, bool) {
	var head *queuedNotification
	for i := range a.Outbox {
		item := &a.Outbox[i]
		if item.workerKey() != key {
			continue
		}
		if head == nil {
			head = item
			continue
		}
		if !head.Resolved && item.Resolved && !a.sentTo(head.Destination, head.Chain+"\x00"+head.Message, head.Message) {
			return *head, true, true
		}
		break
	}
	if head == nil {
		return queuedNotification{}, false, false
	}
	return *head, true, false
}

func (c *Config) acknowledge(sequence uint64) error {
	alarms.notifyMux.Lock()
	for i := range alarms.Outbox {
		if alarms.Outbox[i].Sequence == sequence {
			alarms.Outbox = append(alarms.Outbox[:i], alarms.Outbox[i+1:]...)
			break
		}
	}
	alarms.notifyMux.Unlock()
	return c.persistState()
}

func (c *Config) deliverQueue(key string) {
	delay := 5 * time.Second
	for c.context().Err() == nil {
		item, exists, skip := c.queuedHead(key)
		if !exists {
			return
		}
		// try to save the queue first, but a broken state file should never hold back an alert
		if !c.queueSaved(item.Sequence) {
			if err := c.persistState(); err != nil {
				l("⚠️ could not save notification queue, sending anyway", err)
			}
		}
		// wait for the group, then read the head again
		if d := c.groupDelay(item); d > 0 && !skip {
			if !waitContext(c.context(), d) {
				return
			}
			continue
		}
		msg := c.makeAlert(item.Chain, item.Message, item.Severity, item.Resolved, &item.ID)
		orphan := false
		if r, ok := storedResolution(item.Chain, item.Message); ok {
			orphan = item.Resolved && r.label == "No longer watched"
		}
		if (msg == nil || orphan) && item.snapshot != nil {
			copy := *item.snapshot
			msg = &copy
		}
		if msg == nil {
			skip = true
		} else {
			msg.lasted, msg.quiet = item.Lasted, item.Quiet && item.Resolved
		}
		var err error
		if !skip {
			msg.ordered = true
			msg.ctx = c.deliveryCtx
			grouped := false
			if !orphan {
				grouped, err = c.deliverGrouped(item, msg)
			}
			if !grouped {
				switch item.Destination {
				case pd:
					if msg.pd {
						err = notifyPagerduty(msg)
					}
				case tg:
					if msg.tg {
						err = notifyTg(msg)
					}
				case di:
					if msg.disc {
						err = notifyDiscord(msg)
					}
				case slk:
					if msg.slk {
						err = notifySlack(msg)
					}
				}
			}
		}
		if err == nil {
			if err := c.acknowledge(item.Sequence); err != nil {
				l("could not save notification acknowledgement:", err)
			}
			delay = 5 * time.Second
			continue
		}
		if !errors.Is(err, errNotificationBusy) {
			l(item.Chain, "notification delivery failed:", err)
		}
		if !waitContext(c.context(), delay) {
			return
		}
		if delay < 5*time.Minute {
			delay *= 2
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
		}
	}
}

func (msg *alertMsg) context() context.Context {
	if msg.ctx != nil {
		return msg.ctx
	}
	return context.Background()
}

type notificationClient struct {
	ctx    context.Context
	client *http.Client
}

func newNotificationClient(ctx context.Context, timeout time.Duration) notificationClient {
	return notificationClient{ctx: ctx, client: &http.Client{Timeout: timeout}}
}

func (client notificationClient) Do(request *http.Request) (*http.Response, error) {
	//#nosec G704 -- webhook URLs come from the operator's config
	resp, err := client.client.Do(request.WithContext(client.ctx))
	return resp, scrubURLError(err)
}

// scrubbedError removes URL paths and queries that may contain credentials.
type scrubbedError struct {
	text string
	err  error
}

func (e *scrubbedError) Error() string { return e.text }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubURLError formats logs as "Post scheme://host: cause".
func scrubURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	text := urlErr.Op
	if u, perr := url.Parse(urlErr.URL); perr == nil && u.Host != "" {
		text += " " + u.Scheme + "://" + u.Host
	}
	return &scrubbedError{text: text + ": " + urlErr.Err.Error(), err: urlErr.Err}
}
