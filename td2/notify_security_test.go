package tenderduty

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestNotificationErrorsHideTokens(t *testing.T) {
	auditConfig(t)
	const token = "123456:FAKE-bot-token_abc"
	// port 1 is expected to refuse connections
	unreachable := "http://127.0.0.1:1/bot" + token + "/sendMessage?key=" + token
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: an unreachable URL succeeded", name)
		}
		text := err.Error()
		if strings.Contains(text, token) || strings.Contains(text, "sendMessage") || strings.Contains(text, "key=") {
			t.Fatalf("%s: the error leaks the URL: %s", name, text)
		}
		if !strings.Contains(text, "http://127.0.0.1:1") {
			t.Fatalf("%s: the error lost the host: %s", name, text)
		}
	}
	client := newNotificationClient(context.Background(), 2*time.Second)
	req, err := http.NewRequest(http.MethodPost, unreachable, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	check("client", err)
	_, err = tgbotapi.NewBotAPIWithClient(token, "http://127.0.0.1:1/bot%s/%s", client)
	check("telegram", err)
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "something happened", slkHook: unreachable, discHook: unreachable}
	check("slack", postSlack(msg))
	check("discord", postDiscord(msg))

	// the cause stays reachable for errors.Is
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = newNotificationClient(ctx, 2*time.Second).Do(req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was lost: %v", err)
	}
	// a URL that does not parse is scrubbed too
	_, err = http.NewRequest(http.MethodPost, "http://hooks.example/"+token+"\x7f", nil)
	if text := scrubURLError(err).Error(); err == nil || strings.Contains(text, token) {
		t.Fatalf("a malformed URL leaks: %v", text)
	}
	if plain := errors.New("not a URL error"); scrubURLError(plain) != plain || scrubURLError(nil) != nil {
		t.Fatal("other errors must pass through")
	}
}

func TestSlackTopLineEscapesChainText(t *testing.T) {
	msg := &alertMsg{chain: "Devnet1", severity: "critical", message: "Proposal #1 <!channel> <!here> & <@U123|admin>"}
	want := "Proposal #1 &lt;!channel&gt; &lt;!here&gt; &amp; &lt;@U123|admin&gt;"
	if text := buildSlackMessage(msg).Text; text != want {
		t.Fatalf("the top line sends slack markup: %q", text)
	}
}

func TestDiscordAllowedMentions(t *testing.T) {
	for _, tc := range []struct{ name, configured, want string }{
		{"none", "", `{"parse":[]}`},
		{"user and role", "<@1234> <@&55>", `{"parse":[],"users":["1234"],"roles":["55"]}`},
		{"nickname form and repeats", "<@!77> <@77> <@!77>", `{"parse":[],"users":["77"]}`},
		{"here", "@here", `{"parse":["everyone"]}`},
		{"everyone with a user", "<@9> @everyone", `{"parse":["everyone"],"users":["9"]}`},
	} {
		for _, style := range []string{"", "plain"} {
			// the chain name and message are chain text, none of it may ping
			msg := &alertMsg{chain: "@everyone <@&99>", severity: "critical", message: "Proposal #1 <@&99> @everyone @here", discMentions: tc.configured, discStyle: style}
			data, err := json.Marshal(buildDiscordMessage(msg))
			if err != nil {
				t.Fatal(err)
			}
			var sent struct {
				AllowedMentions json.RawMessage `json:"allowed_mentions"`
			}
			if err := json.Unmarshal(data, &sent); err != nil || string(sent.AllowedMentions) != tc.want {
				t.Fatalf("%s (%q style): allowed_mentions = %s, want %s", tc.name, style, sent.AllowedMentions, tc.want)
			}
		}
	}
	var many strings.Builder
	for i := 0; i < 150; i++ {
		fmt.Fprintf(&many, "<@%d> ", 1000+i)
	}
	if got := len(discordAllowedMentions(many.String()).Users); got != discordMentionLimit {
		t.Fatalf("%d users sent, discord takes %d", got, discordMentionLimit)
	}
}

func TestGovernanceTitleIsPlainAndShort(t *testing.T) {
	cc := &ChainConfig{}
	end := time.Date(2030, 1, 2, 3, 4, 0, 0, time.UTC)
	message := cc.govOpenMessage(govProposal{ID: 7, Title: "Big\r\nvote\x00\x1b[31m @everyone\u0085next\ttab", VotingEnd: end})
	lines := strings.Split(message, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "Proposal #7 Big") || !strings.HasPrefix(lines[1], "Ends ") {
		t.Fatalf("a title added lines: %q", message)
	}
	for _, r := range message {
		if r < ' ' && r != '\n' || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			t.Fatalf("control character %U in %q", r, message)
		}
	}
	long := cc.govOpenMessage(govProposal{ID: 8, Title: strings.Repeat("é", 500), VotingEnd: end})
	title := strings.TrimPrefix(strings.SplitN(long, "\n", 2)[0], "Proposal #8 ")
	if n := utf8.RuneCountInString(title); n != 200 {
		t.Fatalf("title is %d runes, want 200", n)
	}
	if got := cc.govOpenMessage(govProposal{ID: 9, Title: " \n\t ", VotingEnd: end}); strings.HasPrefix(got, "Proposal #9 \n") || !strings.HasPrefix(got, "Proposal #9\nEnds") {
		t.Fatalf("a blank title left a gap: %q", got)
	}
}
