// Toyzz Shop stok izleyici (Go + surf). Her ürünün virtual_stock'unu kontrol eder;
// 0 -> >0 geçişinde Telegram'a bildirir. Tek surf client reuse edilir.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/enetx/g"
	"github.com/enetx/surf"
)

type Product struct {
	PID    int
	Serial int
	Name   string
	URL    string
}

var products = []Product{
	{60065, 104377, "FIFA World Cup 2026 Çıkartma Paketi", "https://www.toyzzshop.com/fifa-world-cup-2026-cikartma-paketi?serial=104377"},
	{60066, 104378, "FIFA World Cup 2026 Çıkartma Albümü", "https://www.toyzzshop.com/fifa-world-cup-2026-cikartma-albumu?serial=104378"},
	{60067, 104379, "FIFA World Cup 2026 Sert Kapak Çıkartma Albümü", "https://www.toyzzshop.com/fifa-world-cup-2026-sert-kapak-100lu-cikartma-albumu?serial=104379"},
}

var (
	interval  = envInt("CHECK_INTERVAL_SECONDS", 60)
	stateFile = env("STATE_FILE", "/data/state.json")
	botToken  = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID    = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	threadID  = strings.TrimSpace(os.Getenv("TELEGRAM_THREAD_ID"))
)

var appHeaders = []any{
	"Accept", "application/json, text/plain, */*",
	"Accept-Language", "tr-TR,tr;q=0.9,en;q=0.8",
	"Origin", "https://www.toyzzshop.com",
	"Referer", "https://www.toyzzshop.com/",
}

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

type apiResp struct {
	Payload struct {
		Product struct {
			VirtualStock *float64 `json:"virtual_stock"`
		} `json:"product"`
	} `json:"payload"`
}

// fetchStock: stok adedi ve okunup okunamadığı
func fetchStock(cli *surf.Client, p Product) (int, bool) {
	url := fmt.Sprintf("https://core.toyzzshop.com/api/products/view/%d/%d", p.PID, p.Serial)
	r := cli.Get(g.String(url)).Do()
	if r.IsErr() {
		logf("API hata pid=%d: %v", p.PID, r.Err())
		return 0, false
	}
	if !r.Ok().StatusCode.IsSuccess() {
		logf("API hata pid=%d: status %d", p.PID, int(r.Ok().StatusCode))
		return 0, false
	}
	var data apiResp
	if json.Unmarshal([]byte(r.Ok().Body.String().Unwrap()), &data) != nil || data.Payload.Product.VirtualStock == nil {
		return 0, false
	}
	return int(*data.Payload.Product.VirtualStock), true
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
	var raw struct {
		Stocks      map[string]*float64 `json:"stocks"`
		LastChecked int64               `json:"last_checked"`
	}
	if json.Unmarshal(b, &raw) != nil {
		logf("State dosyasi bozuk, sifirdan basliyorum")
		return s
	}
	for k, v := range raw.Stocks {
		if v != nil {
			s.Stocks[k] = int(*v)
		}
	}
	s.LastChecked = raw.LastChecked
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
	hb.WriteString("🟢 Toyzz Shop stok botu ayakta — çalışıyor.\n🧩 İzlenen ürünler:")
	for _, p := range products {
		hb.WriteString("\n   • " + p.Name)
	}
	hb.WriteString("\nStok geldikçe ürün linkiyle buraya yazacağım.")
	sendTelegram(hb.String())
	logf("Acilis bildirimi gonderildi.")

	for {
		for _, p := range products {
			key := fmt.Sprintf("%d:%d", p.PID, p.Serial)
			stock, ok := fetchStock(cli, p)
			if !ok {
				logf("%s: stok okunamadi", p.Name)
				continue
			}
			prev, had := state.Stocks[key]
			logf("%s: virtual_stock=%d (onceki=%v)", p.Name, stock, prev)
			if stock > 0 && (!had || prev <= 0) {
				if sendTelegram(fmt.Sprintf("🚨 YENİ STOK! — %s\n🔗 %s\n🏪 Toyzz Shop (online) — %d adet", p.Name, p.URL, stock)) {
					logf("Stok bildirimi gonderildi: %s", p.Name)
				}
			}
			state.Stocks[key] = stock
		}
		state.LastChecked = time.Now().Unix()
		saveState(state)
		time.Sleep(time.Duration(interval) * time.Second)
	}
}
