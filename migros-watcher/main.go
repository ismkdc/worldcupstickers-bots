// Migros "Tıkla Gel Al" mağaza stok izleyici (Go + surf).
// Davranış Python sürümüyle birebir: her mağaza için POST select -> GET screen
// -> IN_SALE ise PUT cart ile stok adedi. Worker başına tek surf client
// (Chrome impersonate + cookie jar) => bağlantı/TLS/DNS yeniden kullanılır.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/enetx/g"
	"github.com/enetx/surf"
)

// ---------------------------------------------------------------- ürünler
type Product struct {
	Full  int64
	Short string
	Name  string
	URL   string
}

var products = []Product{
	{20000037458885, "37458885", "Çoklu Çıkartma Worldcup26", "https://www.migros.com.tr/coklu-cikrt-worldcup26-p-23b93c5"},
	{20000037457979, "37457979", "Süper Çoklu Worldcup 2026", "https://www.migros.com.tr/super-cokluworldcup-2026-p-23b903b"},
	{20000037458884, "37458884", "Çıkartma Albüm Worldcup26", "https://www.migros.com.tr/cikrt-album-worldcup26-p-23b93c4"},
	{20000037457978, "37457978", "Çıkartma Paketi Worldcup 2026", "https://www.migros.com.tr/cikartma-paketi-worldcup-2026-p-23b903a"},
}

func prodByShort(short string) Product {
	for _, p := range products {
		if p.Short == short {
			return p
		}
	}
	return Product{}
}

type Store struct {
	PickPointID int64   `json:"pickPointId"`
	StoreID     int64   `json:"storeId"`
	Name        string  `json:"name"`
	Town        string  `json:"town"`
	City        string  `json:"city"`
	Address     string  `json:"address"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
}

// ---------------------------------------------------------------- config
var (
	interval   = envInt("CHECK_INTERVAL_SECONDS", 60)
	workers    = envInt("WORKERS", 8)
	stateFile  = env("STATE_FILE", "/data/state.json")
	storesFile = env("STORES_FILE", "/app/target_stores.json")
	botToken   = strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN"))
	chatID     = strings.TrimSpace(os.Getenv("TELEGRAM_CHAT_ID"))
	threadID   = strings.TrimSpace(os.Getenv("TELEGRAM_THREAD_ID"))
)

const base = "https://www.migros.com.tr/rest"

var appHeaders = []any{
	"accept", "application/json",
	"accept-language", "tr",
	"origin", "https://www.migros.com.tr",
	"referer", "https://www.migros.com.tr/",
	"x-pwa", "true",
	"x-device-pwa", "true",
	"x-forwarded-rest", "true",
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

// ---------------------------------------------------------------- request body
type selectBody struct {
	AddressID             *int   `json:"addressId"`
	ServiceAreaObjectID   int64  `json:"serviceAreaObjectId"`
	ServiceAreaObjectType string `json:"serviceAreaObjectType"`
}

type cartItem struct {
	ProductID int64  `json:"productId"`
	StoreID   int64  `json:"storeId"`
	Unit      string `json:"unit"`
	Amount    int    `json:"amount"`
}

type cartBody struct {
	Items                     []cartItem `json:"items"`
	ApplyCrmDiscounts         bool       `json:"applyCrmDiscounts"`
	ApplySpecialDiscounts     bool       `json:"applySpecialDiscounts"`
	ApplyDeepDiscounts        bool       `json:"applyDeepDiscounts"`
	IncludeDeliveryFee        bool       `json:"includeDeliveryFee"`
	ApplyFoodProductDiscounts bool       `json:"applyFoodProductDiscounts"`
}

// ---------------------------------------------------------------- response
type screenResp struct {
	Data struct {
		StoreProductInfoDTO struct {
			Status string `json:"status"`
		} `json:"storeProductInfoDTO"`
	} `json:"data"`
}

type cartResp struct {
	Data struct {
		ItemInfos []struct {
			Item struct {
				ProductID int64 `json:"productId"`
			} `json:"item"`
			Product struct {
				StoreProductInfo struct {
					CurrentStockAmount *float64 `json:"currentStockAmount"`
				} `json:"storeProductInfo"`
			} `json:"product"`
		} `json:"itemInfos"`
	} `json:"data"`
}

type prodRes struct {
	Status string
	Stock  *float64
}

type storeResult struct {
	PickPointID int64
	Products    map[string]prodRes
}

// ---------------------------------------------------------------- HTTP
func newClient() *surf.Client {
	return surf.NewClient().
		Builder().
		Impersonate().Chrome().
		Session().
		SetHeaders(appHeaders...).
		Build().
		Unwrap()
}

func checkStore(cli *surf.Client, st Store) storeResult {
	out := storeResult{PickPointID: st.PickPointID, Products: map[string]prodRes{}}

	sel := cli.Post(g.String(base + "/delivery-bff/preferences/select")).
		Body(selectBody{AddressID: nil, ServiceAreaObjectID: st.PickPointID, ServiceAreaObjectType: "PICK_POINT"}).
		Do()
	if sel.IsErr() {
		return out // transport hatası -> boş (bilinmeyen)
	}

	for _, p := range products {
		res := prodRes{}
		sr := cli.Get(g.String(fmt.Sprintf("%s/products/screens/%s", base, p.Short))).Do()
		if sr.IsOk() {
			var scr screenResp
			if json.Unmarshal([]byte(sr.Ok().Body.String().Unwrap()), &scr) == nil {
				res.Status = scr.Data.StoreProductInfoDTO.Status
				if res.Status == "IN_SALE" {
					cr := cli.Put(g.String(base + "/carts/items")).
						Body(cartBody{Items: []cartItem{{ProductID: p.Full, StoreID: st.StoreID, Unit: "PIECE", Amount: 1}}}).
						Do()
					if cr.IsOk() {
						var cres cartResp
						if json.Unmarshal([]byte(cr.Ok().Body.String().Unwrap()), &cres) == nil {
							for _, it := range cres.Data.ItemInfos {
								if it.Item.ProductID == p.Full {
									res.Stock = it.Product.StoreProductInfo.CurrentStockAmount
									break
								}
							}
						}
					}
				}
			}
		}
		out.Products[p.Short] = res
	}
	return out
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
	ok := true
	for _, chunk := range chunks(text, 3900) {
		payload := tgPayload{ChatID: chatID, Text: chunk, DisableWebPagePreview: true}
		if threadID != "" {
			if tid, err := strconv.Atoi(threadID); err == nil {
				payload.MessageThreadID = tid
			}
		}
		sent := false
		for attempt := 0; attempt < 6; attempt++ {
			r := cli.Post(g.String(url)).Body(payload).Do()
			if r.IsErr() {
				logf("Telegram istek hatasi: %v", r.Err())
				time.Sleep(3 * time.Second)
				continue
			}
			sc := int(r.Ok().StatusCode)
			if sc == 200 {
				sent = true
				break
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
			ok = false
			break
		}
		if !sent {
			ok = false
		}
	}
	return ok
}

func chunks(text string, limit int) []string {
	var out []string
	var buf string
	for _, line := range strings.Split(text, "\n") {
		if len(buf)+len(line)+1 > limit {
			if buf != "" {
				out = append(out, buf)
			}
			buf = line
		} else if buf != "" {
			buf = buf + "\n" + line
		} else {
			buf = line
		}
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out
}

// ---------------------------------------------------------------- state
type State struct {
	Stores           map[string]int `json:"stores"`
	StartupNotified  bool           `json:"startup_notified"`
	LastBaselineDate string         `json:"last_baseline_date"`
	LastChecked      int64          `json:"last_checked"`
}

func loadState() State {
	s := State{Stores: map[string]int{}}
	b, err := os.ReadFile(stateFile)
	if err != nil {
		return s
	}
	// toleranslı: "stores" değerleri null olabilir (IN_SALE ama stok okunamamış)
	var raw struct {
		Stores           map[string]*float64 `json:"stores"`
		StartupNotified  bool                `json:"startup_notified"`
		LastBaselineDate string              `json:"last_baseline_date"`
		LastChecked      int64               `json:"last_checked"`
	}
	if json.Unmarshal(b, &raw) != nil {
		logf("State bozuk, sifirdan basliyorum")
		return s
	}
	for k, v := range raw.Stores {
		s.Stores[k] = ptrOr0(v)
	}
	s.StartupNotified = raw.StartupNotified
	s.LastBaselineDate = raw.LastBaselineDate
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

func loadStores() []Store {
	b, err := os.ReadFile(storesFile)
	if err != nil {
		logf("ERROR stores okunamadi: %v", err)
		os.Exit(2)
	}
	var s []Store
	if json.Unmarshal(b, &s) != nil {
		logf("ERROR stores parse edilemedi")
		os.Exit(2)
	}
	return s
}

// ---------------------------------------------------------------- scan
func scan(stores []Store) map[int64]storeResult {
	jobs := make(chan Store, len(stores))
	for _, s := range stores {
		jobs <- s
	}
	close(jobs)

	results := make(chan storeResult, len(stores))
	var wg sync.WaitGroup
	var done int64
	t0 := time.Now()
	logf("Tarama basladi: %d magaza x %d urun, %d worker", len(stores), len(products), workers)

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cli := newClient() // worker basina tek client -> baglanti reuse
			for st := range jobs {
				results <- checkStore(cli, st)
				n := atomic.AddInt64(&done, 1)
				if n%50 == 0 || n == int64(len(stores)) {
					logf("  tarama %d/%d · %.0fsn", n, len(stores), time.Since(t0).Seconds())
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()

	out := make(map[int64]storeResult, len(stores))
	for r := range results {
		out[r.PickPointID] = r
	}
	return out
}

func fmtStock(p *float64) string {
	if p == nil {
		return "?"
	}
	return strconv.Itoa(int(*p))
}

type entry struct {
	Store Store
	Stock *float64
}

func productMsg(p Product, entries []entry, header string) string {
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i].Store, entries[j].Store
		if a.City != b.City {
			return a.City < b.City
		}
		if a.Town != b.Town {
			return a.Town < b.Town
		}
		return a.Name < b.Name
	})
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%s — %s\n🔗 %s\n🏬 Stoktaki mağazalar (%d):", header, p.Name, p.URL, len(entries)))
	for _, e := range entries {
		b.WriteString(fmt.Sprintf("\n   • %s — %s/%s — %s adet", e.Store.Name, e.Store.Town, e.Store.City, fmtStock(e.Stock)))
	}
	return b.String()
}

// ---------------------------------------------------------------- main
func main() {
	if botToken == "" || chatID == "" {
		logf("ERROR: TELEGRAM_BOT_TOKEN ve TELEGRAM_CHAT_ID gerekli.")
		os.Exit(2)
	}

	stores := loadStores()
	byCity := map[string]bool{}
	for _, s := range stores {
		c := s.City
		if c == "" {
			c = "?"
		}
		byCity[c] = true
	}
	meta := make(map[int64]Store, len(stores))
	for _, s := range stores {
		meta[s.PickPointID] = s
	}
	total := len(stores)
	logf("Baslatildi. %d magaza, %d il, aralik=%ds", total, len(byCity), interval)

	state := loadState()

	// açılış heartbeat
	var hb strings.Builder
	hb.WriteString("🟢 Migros stok botu ayakta — çalışıyor.\n🧩 İzlenen ürünler:")
	for _, p := range products {
		hb.WriteString("\n   • " + p.Name)
	}
	hb.WriteString(fmt.Sprintf("\n🇹🇷 Tüm Türkiye: %d mağaza / %d il izleniyor", total, len(byCity)))
	hb.WriteString("\n⏱ Tam tarama birkaç dk sürer · stok geldikçe buraya yazacağım.")
	sendTelegram(hb.String())
	logf("Acilis bildirimi gonderildi.")

	for {
		// günlük 09:00 TSI baseline reset
		now3 := time.Now().UTC().Add(3 * time.Hour)
		today := now3.Format("2006-01-02")
		if now3.Hour() >= 9 && state.LastBaselineDate != today {
			logf("Gunluk 09:00 TSI reset -> baseline yeniden kuruluyor (%s)", today)
			state.Stores = map[string]int{}
			state.StartupNotified = false
			state.LastBaselineDate = today
			saveState(state)
		}

		results := scan(stores)

		// IN_SALE olanlar
		type triple struct {
			st   Store
			prod Product
			res  prodRes
		}
		var inStock []triple
		for pp, r := range results {
			for short, pr := range r.Products {
				if pr.Status == "IN_SALE" {
					inStock = append(inStock, triple{meta[pp], prodByShort(short), pr})
				}
			}
		}

		// ilk tam tarama: baseline + ürün bazında gruplu özet
		if !state.StartupNotified {
			byProd := map[string][]entry{}
			for _, t := range inStock {
				byProd[t.prod.Short] = append(byProd[t.prod.Short], entry{t.st, t.res.Stock})
			}
			if len(byProd) > 0 {
				sendTelegram(fmt.Sprintf("✅ Migros ilk tarama tamam — %d mağaza × %d ürün. Şu an stoktaki ürünler aşağıda 👇", total, len(products)))
				for _, p := range products {
					if e, ok := byProd[p.Short]; ok {
						sendTelegram(productMsg(p, e, "🟢 STOKTA"))
					}
				}
			} else {
				sendTelegram(fmt.Sprintf("✅ Migros ilk tarama tamam — %d mağaza × %d ürün tarandı, şu an hiçbirinde stok yok. Stok geldikçe buraya yazacağım.", total, len(products)))
			}
			for pp, r := range results {
				for short, pr := range r.Products {
					if pr.Status == "IN_SALE" {
						byCityKey := fmt.Sprintf("%d:%s", pp, short)
						state.Stores[byCityKey] = ptrOr0(pr.Stock)
					} else {
						state.Stores[fmt.Sprintf("%d:%s", pp, short)] = 0
					}
				}
			}
			state.StartupNotified = true
			saveState(state)
			logf("Ilk tarama baseline kuruldu.")
			time.Sleep(time.Duration(interval) * time.Second)
			continue
		}

		// normal: yok -> var geçişleri
		transitions := map[string][]entry{}
		for pp, r := range results {
			st := meta[pp]
			for short, pr := range r.Products {
				key := fmt.Sprintf("%d:%s", pp, short)
				cur := 0
				if pr.Status == "IN_SALE" && pr.Stock != nil && *pr.Stock > 0 {
					cur = int(*pr.Stock)
				}
				prev := state.Stores[key]
				if cur > 0 && prev <= 0 {
					transitions[short] = append(transitions[short], entry{st, pr.Stock})
				}
				if pr.Status != "" {
					state.Stores[key] = cur
				}
			}
		}
		for _, p := range products {
			if e, ok := transitions[p.Short]; ok {
				sendTelegram(productMsg(p, e, "🚨 YENİ STOK!"))
				logf("STOK bildirimi: %s (%d magaza)", p.Name, len(e))
			}
		}

		state.LastChecked = time.Now().Unix()
		saveState(state)
		logf("Tur bitti. stokta(urun-magaza)=%d", len(inStock))
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

func ptrOr0(p *float64) int {
	if p == nil {
		return 0
	}
	return int(*p)
}
