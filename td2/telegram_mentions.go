package tenderduty

import (
	"fmt"
	"html"
	"sort"
	"strings"
	"sync"
)

// tgMention stores /mentionme users by ID, including those without usernames.
type tgMention struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

var (
	tgMentionMux sync.RWMutex
	tgMentions   []tgMention
)

// link pings by user id, which works without a username.
func (m tgMention) link() string {
	return fmt.Sprintf(`<a href="tg://user?id=%d">%s</a>`, m.ID, html.EscapeString(m.Name))
}

// telegramMentions returns escaped config names and user links as HTML.
func telegramMentions(configured []string) string {
	parts := make([]string, 0, len(configured))
	for _, name := range configured {
		if name = strings.TrimSpace(name); name != "" {
			parts = append(parts, html.EscapeString(name))
		}
	}
	tgMentionMux.RLock()
	defer tgMentionMux.RUnlock()
	for _, m := range tgMentions {
		parts = append(parts, m.link())
	}
	return strings.Join(parts, " ")
}

func mentionSnapshot() []tgMention {
	tgMentionMux.RLock()
	defer tgMentionMux.RUnlock()
	return append([]tgMention(nil), tgMentions...)
}

func restoreMentions(saved []tgMention) {
	tgMentionMux.Lock()
	defer tgMentionMux.Unlock()
	tgMentions = append([]tgMention(nil), saved...)
}

// addMention records or renames someone, and says if anything changed.
func addMention(id int64, name string) bool {
	if id == 0 {
		return false
	}
	tgMentionMux.Lock()
	defer tgMentionMux.Unlock()
	for i, m := range tgMentions {
		if m.ID == id {
			if m.Name == name {
				return false
			}
			tgMentions[i].Name = name
			return true
		}
	}
	tgMentions = append(tgMentions, tgMention{ID: id, Name: name})
	sort.Slice(tgMentions, func(i, j int) bool { return tgMentions[i].ID < tgMentions[j].ID })
	return true
}

func (c *Config) saveMentions() {
	if err := c.persistState(); err != nil {
		l("could not save the telegram ping list:", err)
	}
}

func removeMention(id int64) bool {
	tgMentionMux.Lock()
	defer tgMentionMux.Unlock()
	for i, m := range tgMentions {
		if m.ID == id {
			tgMentions = append(tgMentions[:i], tgMentions[i+1:]...)
			return true
		}
	}
	return false
}
