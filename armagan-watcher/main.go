// Armağan Oyuncak stok izleyici (Go + surf). Ürün sayfasındaki schema.org
// JSON-LD'sinden offers.availability okunur; OutOfStock -> InStock geçişinde
// Telegram'a bildirilir. Tek surf client reuse edilir.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/enetx/g"
	"github.com/enetx/surf"
)

type Product struct {
	Name string
	URL  string
}

var products = []Product{
	{"FIFA World Cup 2026 Çıkartma Albümü", "https://www.armaganoyuncak.com.tr/fifa-world-cup-2026-cikartma-albumu-p-010101bas02943"},
	{"FIFA World Cup 2026 Çıkartma Paketi", "https://www.armaganoyuncak.com.tr/fifa-world-cup-2026-cikartma-paketi-p-010101bas08031"},
}

var (
	interval  = envInt("CHECK_INTERVAL_SECONDS", 60)
	stateFile = env("STATE_FILE", "/data/state.json")
	botToken  = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID    = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	threadID  = strings.TrimSpace(os.Getenv("TELEGRAM_THREAD_ID"))
)

var appHeaders = []any{
	"accept-language", "tr-TR,tr;q=0.9",
}

var ldRe = regexp.MustCompile(`(?s)<script[^>]*type="application/ld\+json"[^>]*>(.*?)</script>`)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func logf(format string, a ...any) {
	fmt.Printf(time.Now().Format("2006-01-02 15:04:05")+" "+format+"\n", a...)
}

func newClient() *surf.Client {
	return surf.NewClient().Builder().
		Impersonate().Chrome().
		SetHeaders(appHeaders...).
		Build().
		Unwrap()
}

func parsePrice(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return &f
		}
	}
	return nil
}

// fetchStock: JSON-LD Product.offers.availability -> (instock *bool, price *float64)
func fetchStock(cli *surf.Client, url string) (*bool, *float64) {
	r := cli.Get(g.String(url)).Do()
	if r.IsErr() {
		logf("fetch hata: %v", r.Err())
		return nil, nil
	}
	html := string(r.Ok().Body.String().Unwrap())
	for _, m := range ldRe.FindAllStringSubmatch(html, -1) {
		var v any
		if json.Unmarshal([]byte(strings.TrimSpace(m[1])), &v) != nil {
			continue
		}
		var items []any
		if arr, ok := v.([]any); ok {
			items = arr
		} else {
			items = []any{v}
		}
		for _, it := range items {
			mp, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := mp["@type"].(string); t != "Product" {
				continue
			}
			var offMap map[string]any
			switch off := mp["offers"].(type) {
			case map[string]any:
				offMap = off
			case []any:
				if len(off) > 0 {
					offMap, _ = off[0].(map[string]any)
				}
			}
			if offMap == nil {
				continue
			}
			avail, _ := offMap["availability"].(string)
			instock := strings.Contains(avail, "InStock")
			return &instock, parsePrice(offMap["price"])
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- Telegram
type tgPayload struct {
	ChatID                string `json:"chat_id"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
	MessageThreadID       int    `json:"message_thread_id,omitempty"`
}

func sendTelegram(text string) bool {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", botToken)
	cli := surf.NewClient()
	payload := tgPayload{ChatID: chatID, Text: text, DisableWebPagePreview: true}
	if threadID != "" {
		if tid, err := strconv.Atoi(threadID); err == nil {
			payload.MessageThreadID = tid
		}
	}
	for attempt := 0; attempt < 6; attempt++ {
		r := cli.Post(g.String(url)).Body(payload).Do()
		if r.IsErr() {
			logf("Telegram istek hatasi: %v", r.Err())
			time.Sleep(3 * time.Second)
			continue
		}
		sc := int(r.Ok().StatusCode)
		if sc == 200 {
			return true
		}
		if sc == 429 {
			wait := 5
			var er struct {
				Parameters struct {
					RetryAfter int `json:"retry_after"`
				} `json:"parameters"`
			}
			if json.Unmarshal([]byte(r.Ok().Body.String().Unwrap()), &er) == nil && er.Parameters.RetryAfter > 0 {
				wait = er.Parameters.RetryAfter
			}
			logf("Slow-mode/429: %ss bekleniyor (deneme %d)", wait, attempt+1)
			time.Sleep(time.Duration(wait+1) * time.Second)
			continue
		}
		body := r.Ok().Body.String().Unwrap()
		if len(body) > 200 {
			body = body[:200]
		}
		logf("Telegram hata: %d %s", sc, body)
		return false
	}
	logf("Telegram: retry tukendi, gonderilemedi")
	return false
}

// ---------------------------------------------------------------- state
type State struct {
	Stocks      map[string]int `json:"stocks"`
	LastChecked int64          `json:"last_checked"`
}

func loadState() State {
	s := State{Stocks: map[string]int{}}
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return s
	}
	if json.Unmarshal(b, &s) != nil {
		logf("State bozuk, sifirdan basliyorum")
		return State{Stocks: map[string]int{}}
	}
	if s.Stocks == nil {
		s.Stocks = map[string]int{}
	}
	return s
}

func saveState(s State) {
	os.MkdirAll(filepath.Dir(stateFile), 0o755)
	b, _ := json.Marshal(s)
	tmp := stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, stateFile)
	}
}

// ---------------------------------------------------------------- main
func main() {
	if botToken == "" || chatID == "" {
		logf("ERROR: TELEGRAM_BOT_TOKEN ve TELEGRAM_CHAT_ID gerekli.")
		os.Exit(2)
	}
	logf("Baslatildi. Aralik=%ds, %d urun izleniyor", interval, len(products))
	state := loadState()
	cli := newClient()

	var hb strings.Builder
	hb.WriteString("🟢 Armağan Oyuncak stok botu ayakta — çalışıyor.\n🧩 İzlenen ürünler:")
	for _, p := range products {
		hb.WriteString("\n   • " + p.Name)
	}
	hb.WriteString("\nStok geldikçe ürün linkiyle buraya yazacağım.")
	sendTelegram(hb.String())
	logf("Acilis bildirimi gonderildi.")

	for {
		for _, p := range products {
			instock, price := fetchStock(cli, p.URL)
			if instock == nil {
				logf("%s: stok okunamadi", p.Name)
				continue
			}
			prev := state.Stocks[p.URL] // yoksa 0
			logf("%s: in_stock=%v (onceki=%d)", p.Name, *instock, prev)
			if *instock && prev <= 0 {
				fiyat := ""
				if price != nil {
					fiyat = fmt.Sprintf(" — %d TL", int(*price))
				}
				if sendTelegram(fmt.Sprintf("🚨 YENİ STOK! — %s\n🔗 %s\n🏪 Armağan Oyuncak (online)%s", p.Name, p.URL, fiyat)) {
					logf("Stok bildirimi gonderildi: %s", p.Name)
				}
			}
			if *instock {
				state.Stocks[p.URL] = 1
			} else {
				state.Stocks[p.URL] = 0
			}
		}
		state.LastChecked = time.Now().Unix()
		saveState(state)
		time.Sleep(time.Duration(interval) * time.Second)
	}
}
