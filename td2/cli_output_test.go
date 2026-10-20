package tenderduty

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestFibreSetupRegistration(t *testing.T) {
	for _, mode := range []string{"found", "missing", "override", "control-host", "module-missing", "network-error", "validator-error"} {
		t.Run(mode, func(t *testing.T) {
			options, filename, original := chainCommandFixture(t)
			if mode == "override" {
				original = append(original, []byte("valcons_override: celestiavalcons1override\n")...)
				if err := os.WriteFile(filename, original, 0600); err != nil {
					t.Fatal(err)
				}
			}
			options.reload = func(string) (int, error) { return 123, nil }
			validatorCalls, providerCalls := 0, 0
			fetch := addChainFetchers{
				validator: func(_ context.Context, node, address string) (*ValInfo, error) {
					validatorCalls++
					if node != "https://rpc.example:443" || address != "celestiavaloper1test" {
						t.Fatalf("lookup %s %s", node, address)
					}
					if mode == "validator-error" {
						return nil, errors.New("offline")
					}
					return &ValInfo{Valcons: "celestiavalcons1test"}, nil
				},
				fibreProvider: func(_ context.Context, node, valcons string) (string, bool, error) {
					providerCalls++
					want := "celestiavalcons1test"
					if mode == "override" {
						want = "celestiavalcons1override"
					}
					if node != "https://rpc.example:443" || valcons != want {
						t.Fatalf("provider lookup %s %s", node, valcons)
					}
					switch mode {
					case "missing":
						return "", false, nil
					case "module-missing":
						return "", false, &abciError{codespace: "sdk", code: 6}
					case "network-error":
						return "", false, errors.New("offline")
					}
					if mode == "control-host" {
						return "http://127.0.0.1:1234\x1b", true, nil
					}
					return "http://127.0.0.1:1234", true, nil
				},
			}
			var output bytes.Buffer
			if err := setupFibreWithFetch(context.Background(), options, "Celestia", "", "", false, false, &output, fetch); err != nil {
				t.Fatal(err)
			}
			text := output.String()
			found := mode == "found" || mode == "override" || mode == "control-host"
			if strings.Contains(text, "registered Fibre provider at http://127.0.0.1:1234\n") != found || strings.Contains(text, "\x1b") {
				t.Fatal(text)
			}
			if strings.Contains(text, "Celestia is not a registered Fibre provider yet; the not registered alert fires until it is") != (mode == "missing") ||
				strings.Contains(text, "chain has no x/valaddr support") != (mode == "module-missing") {
				t.Fatal(text)
			}
			wantValidator, wantProvider := 1, 1
			if mode == "override" {
				wantValidator = 0
			}
			if mode == "validator-error" {
				wantProvider = 0
			}
			if validatorCalls != wantValidator || providerCalls != wantProvider {
				t.Fatalf("validator calls %d, provider calls %d", validatorCalls, providerCalls)
			}
			data, err := os.ReadFile(filename)
			if err != nil || !bytes.Contains(data, []byte("fibre_enabled: true")) || !strings.Contains(text, "Fibre alerts enabled") {
				t.Fatalf("%s / %v / %s", data, err, text)
			}
			if mode == "found" && !strings.Contains(text, "--otel-endpoint http://127.0.0.1:4318") {
				t.Fatal(text)
			}
			if strings.Count(text, "tenderduty config set") != 1 || strings.Contains(text, "Fibre metrics need a token") || strings.Contains(text, "Run first:") {
				t.Fatal(text)
			}
		})
	}
}

func TestMetricsSetupAdvice(t *testing.T) {
	for _, bridge := range []bool{false, true} {
		var output bytes.Buffer
		printMetricsSetup(FibreMetricsConfig{}, "http://127.0.0.1:4318", "chain-1", bridge, &output)
		if strings.Contains(output.String(), "Run first:") != bridge {
			t.Fatal(output.String())
		}
	}
}

func TestChainRenameSameName(t *testing.T) {
	options, filename, original := chainCommandFixture(t)
	options.reload = func(string) (int, error) {
		t.Fatal("unexpected reload")
		return 0, nil
	}
	var output bytes.Buffer
	if err := ChainCommand(context.Background(), options, []string{"rename", "Celestia", "Celestia"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil || !bytes.Equal(data, original) || output.String() != "Celestia is already named Celestia\n" {
		t.Fatalf("%s / %v / %s", data, err, &output)
	}
}

func TestDiscordMaskedLinks(t *testing.T) {
	link := "Claim [airdrop](https://evil.example)"
	card := alertCard{Title: link, Note: link, Fields: []alertField{row("", "Validator", link+" \u00b7 9m left"), codeField("", "Host", link)}}
	embed := card.discordEmbed(0)
	for _, text := range []string{strings.Join(card.markdownLines()[:2], "\n") + "\n" + card.markdownLines()[3], embed.Description, embed.Fields[0].Value} {
		if !strings.Contains(text, `\[airdrop](`) || strings.Contains(strings.ReplaceAll(text, `\[airdrop](`, ""), `[airdrop](`) {
			t.Fatal(text)
		}
	}
	if embed.Fields[1].Value != "`"+link+"`" || !strings.Contains(embed.Fields[0].Value, "**9m left**") {
		t.Fatalf("%+v", embed.Fields)
	}
	for _, text := range []string{strings.Join(card.telegramLines(), "\n"), strings.Join(card.plainLines(), "\n"), card.slackFields()[0].Value} {
		if !strings.Contains(text, link) || strings.Contains(text, `\[airdrop](`) {
			t.Fatal(text)
		}
	}
}

func TestDiscordMaskedLinksMessage(t *testing.T) {
	for _, message := range []string{"Proposal #7 Claim [airdrop](https://evil.example)\nEnds 10 Oct 2026 17:48 UTC", "Claim [airdrop](https://evil.example)"} {
		msg := &alertMsg{chain: "Celestia", severity: "warning", message: message}
		for _, style := range []string{"embed", "plain"} {
			msg.discStyle = style
			sent := buildDiscordMessage(msg)
			text := sent.Content
			for _, embed := range sent.Embeds {
				text += embed.Description
				for _, field := range embed.Fields {
					text += field.Value
				}
			}
			if !strings.Contains(text, `\[airdrop](`) || strings.Contains(strings.ReplaceAll(text, `\[airdrop](`, ""), `[airdrop](`) {
				t.Fatalf("%s: %s", style, text)
			}
		}
	}
}
