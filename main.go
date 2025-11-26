package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// ---------------- CONFIG ----------------
const (
	BASE_COLLECTION_URL = "https://www.funcorp.in/collections/hot-wheels"
	// pages to scan (will stop early if a page has no products)
	MAX_PAGES            = 20
	WORKERS              = 5
	CHECK_INTERVAL_SECS  = 90 // default check interval (seconds)
	TELEGRAM_BOT_TOKEN   = "8200088959:AAEv05nLhbWyDGgzbeBN3c-5ersoQ1qanbc"
	TELEGRAM_CHAT_ID     = "-4985438208" // channel/chat to send new item alerts
	ADMIN_CHAT_ID        = "837428747"   // admin control
	SEEN_ITEMS_FILE_FUNC = "seen_funcorp.txt"
	APP_KEEPALIVE_URL    = "https://hh3.onrender.com" 
)

// ---------------- Shared state ----------------
var (
	mutex          sync.Mutex
	checkInterval  = time.Duration(CHECK_INTERVAL_SECS) * time.Second
	isPaused       = false
	heartbeatMuted = false
	seenItems      = make(map[string]bool) // keys: SKU (preferred) or product URL fallback
	checkHistory   []CheckResult
	httpClient     = &http.Client{Timeout: 20 * time.Second}
)

// ---------------- Types ----------------
type FuncorpProduct struct {
	Title       string
	SKU         string
	Price       string
	StockStatus string // e.g. "Sold Out" or ""/Add to cart
	URL         string
	Image       string
}

type CheckResult struct {
	Timestamp     time.Time
	FoundProducts []FuncorpProduct
}

// Telegram update structs (for command listener)
type TelegramUpdateResponse struct {
	Ok     bool     `json:"ok"`
	Result []Update `json:"result"`
}
type Update struct {
	UpdateID int     `json:"update_id"`
	Message  Message `json:"message"`
}
type Message struct {
	Text string `json:"text"`
	Chat Chat   `json:"chat"`
}
type Chat struct {
	ID int64 `json:"id"`
}

// ---------------- Helpers ----------------
func sendTelegramMessage(chatID, message string) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/sendMessage", TELEGRAM_BOT_TOKEN)
	payload := url.Values{}
	payload.Set("chat_id", chatID)
	payload.Set("text", message)
	payload.Set("parse_mode", "HTML")
	resp, err := http.PostForm(apiURL, payload)
	if err != nil {
		log.Printf("❌ Failed to send Telegram message: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("❌ Telegram API Error (Status %d): %s", resp.StatusCode, string(body))
	}
}

func loadSeenItems(filename string) {
	data, _ := os.ReadFile(filename)
	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			seenItems[line] = true
		}
	}
}

func saveNewItem(filename, key string) {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		log.Printf("Error opening file for writing: %v", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(key + "\n"); err != nil {
		log.Printf("Error writing to file: %v", err)
	}
}

func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// fetch product detail page to extract SKU & stock reliably
func fetchProductDetails(productURL string) (sku string, stock string, image string, err error) {
	resp, err := httpClient.Get(productURL)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", "", fmt.Errorf("status %d", resp.StatusCode)
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return "", "", "", err
	}

	// SKU extraction: Look for "SKU:" text in the page
	// Often in a .product__text or similar
	doc.Find("*").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if strings.Contains(s.Text(), "SKU:") {
			// Try to get the text of this element
			t := clean(s.Text())
			// Regex to find "SKU: <value>"
			re := regexp.MustCompile(`SKU:\s*([A-Za-z0-9\-_]+)`)
			if m := re.FindStringSubmatch(t); m != nil {
				sku = m[1]
				return false // stop
			}
		}
		return true
	})

	if sku == "" {
		// Fallback: try common classes
		sku = clean(doc.Find(`span.sku, .product__sku`).First().Text())
	}

	// Stock: Check for Add to Cart button
	if doc.Find("button#product-add-to-cart").Length() > 0 {
		// Check if disabled
		if _, disabled := doc.Find("button#product-add-to-cart").Attr("disabled"); !disabled {
			stock = "Available"
		} else {
			stock = "Sold out"
		}
	} else {
		// Fallback text check
		if strings.Contains(strings.ToLower(doc.Text()), "sold out") {
			stock = "Sold out"
		}
	}

	// Image: try og:image or first product image
	if img, ok := doc.Find(`meta[property="og:image"]`).Attr("content"); ok {
		image = img
	} else if img, ok := doc.Find(".product__media img").Attr("src"); ok {
		if strings.HasPrefix(img, "//") {
			img = "https:" + img
		}
		image = img
	}

	return sku, stock, image, nil
}

// parse a single collection page, return products found (with detail page fetch)
func scrapeCollectionPage(page int) ([]FuncorpProduct, error) {
	var products []FuncorpProduct
	pageURL := BASE_COLLECTION_URL
	if page > 1 {
		pageURL = fmt.Sprintf("%s?page=%d", BASE_COLLECTION_URL, page)
	}
	req, _ := http.NewRequest("GET", pageURL, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; FuncorpBot/1.0)")
	resp, err := httpClient.Do(req)
	if err != nil {
		return products, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return products, fmt.Errorf("status: %d", resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return products, err
	}

	// Updated selectors based on recent inspection
	// Primary container seems to be .grid__item containing a .card-wrapper
	tiles := doc.Find(".grid__item")
	if tiles.Length() == 0 {
		// fallback
		tiles = doc.Find(".product-item, .card-wrapper")
	}

	if tiles.Length() == 0 {
		return products, nil
	}

	tiles.Each(func(i int, s *goquery.Selection) {
		var p FuncorpProduct

		// Title: .card-title
		p.Title = clean(s.Find(".card-title, .product-item__title").First().Text())
		if p.Title == "" {
			p.Title = clean(s.Find("a.card-link").First().AttrOr("title", ""))
		}

		// Filter out placeholder/template items
		if strings.Contains(strings.ToLower(p.Title), "example product title") {
			return
		}

		// Link: .card-link
		if href, ok := s.Find(".card-link, .product-item__info a").First().Attr("href"); ok {
			link := strings.TrimSpace(href)
			if !strings.HasPrefix(link, "http") {
				link = "https://www.funcorp.in" + link
			}
			p.URL = link
		}

		// Price: .price-item (specific classes only to avoid containers)
		p.Price = clean(s.Find(".price-item--sale").First().Text())
		if p.Price == "" {
			p.Price = clean(s.Find(".price-item--regular").First().Text())
		}

		// Stock badge: .badge--sold-out
		p.StockStatus = clean(s.Find(".badge--sold-out, .sold-out-badge").Text())

		// Image: .card__media img
		if img, ok := s.Find(".card__media img, .product-item__image img").Attr("src"); ok {
			if strings.HasPrefix(img, "//") {
				img = "https:" + img
			}
			p.Image = img
		}

		// If SKU or definitive stock isn't present in the listing tile, fetch detail
		if p.URL != "" {
			// fetch detail page (keeps SKU & final stock)
			sku, stock, image, err := fetchProductDetails(p.URL)
			if err == nil {
				if sku != "" {
					p.SKU = sku
				}
				// prefer precise stock from detail page
				if stock != "" {
					p.StockStatus = stock
				}
				// prefer detail image if found
				if image != "" {
					p.Image = image
				}
			} else {
				// log but continue
				log.Printf("⚠️ Failed to fetch details for %s : %v", p.URL, err)
			}
		}

		// If SKU missing, fallback to product URL as unique key (but SKU preferred)
		if p.SKU == "" && p.URL != "" {
			p.SKU = p.URL
		}

		// Only append if we found something valid
		if p.Title != "" || p.URL != "" {
			products = append(products, p)
		}
	})

	return products, nil
}

// ---------------- Core bot logic ----------------
func initializeBaseline(filename string) {
	log.Println("No baseline file found for Funcorp. Performing initial scan...")
	var initialItems []string
	for page := 1; page <= MAX_PAGES; page++ {
		prods, err := scrapeCollectionPage(page)
		if err != nil {
			log.Printf("Error initial scraping page %d: %v", page, err)
			continue
		}
		if len(prods) == 0 {
			break
		}
		for _, p := range prods {
			// only record in-stock items into baseline, similar to FirstCry logic
			if strings.EqualFold(p.StockStatus, "Sold out") || strings.Contains(strings.ToLower(p.StockStatus), "sold") {
				continue
			}
			initialItems = append(initialItems, p.SKU)
		}
		// small pause to be polite
		time.Sleep(500 * time.Millisecond)
	}
	content := strings.Join(initialItems, "\n")
	_ = os.WriteFile(filename, []byte(content), 0644)
	log.Printf("✅ Baseline created with %d IN-STOCK items.", len(initialItems))
}

func getAllProductsScan() ([]FuncorpProduct, error) {
	var all []FuncorpProduct
	for page := 1; page <= MAX_PAGES; page++ {
		prods, err := scrapeCollectionPage(page)
		if err != nil {
			log.Printf("Error scraping page %d: %v", page, err)
			continue
		}
		if len(prods) == 0 {
			break
		}
		for _, p := range prods {
			all = append(all, p)
		}
		// polite delay
		time.Sleep(300 * time.Millisecond)
	}
	return all, nil
}

func checkForNewItemsFuncorp() []FuncorpProduct {
	log.Printf("🔎 (%s) Checking Funcorp pages...", time.Now().Format("15:04:05"))
	var newFound []FuncorpProduct

	allProducts, err := getAllProductsScan()
	if err != nil {
		log.Printf("❌ Error scanning Funcorp: %v", err)
		sendTelegramMessage(ADMIN_CHAT_ID, fmt.Sprintf("⚠️ Funcorp bot error: %v", err))
		return newFound
	}
	log.Printf("... Total products found this check: %d", len(allProducts))

	for _, p := range allProducts {
		// treat "Sold out" (or similar) as unavailable
		if strings.EqualFold(p.StockStatus, "Sold out") || strings.Contains(strings.ToLower(p.StockStatus), "out of stock") {
			continue
		}
		uniqueID := p.SKU

		mutex.Lock()
		seen := seenItems[uniqueID]
		mutex.Unlock()

		if !seen {
			log.Printf("🚨 NEW FUNCORP ITEM: %s (%s)", p.Title, p.SKU)
			newFound = append(newFound, p)

			// Send nice telegram message
			message := fmt.Sprintf(
				"<b>🔥 New Hot Wheels on Funcorp!</b>\n\n<b>%s</b>\n<b>Price:</b> %s\n<b>Stock:</b> %s\n\n<a href='%s'>View product</a>",
				htmlEscape(p.Title), htmlEscape(p.Price), htmlEscape(p.StockStatus), p.URL,
			)
			sendTelegramMessage(TELEGRAM_CHAT_ID, message)

			// persist
			saveNewItem(SEEN_ITEMS_FILE_FUNC, uniqueID)
			mutex.Lock()
			seenItems[uniqueID] = true
			mutex.Unlock()
		}
	}
	return newFound
}

func scraperWorker(stop chan struct{}) {
	initialFinds := checkForNewItemsFuncorp()
	mutex.Lock()
	checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: funcorpProductsToGeneric(initialFinds)})
	mutex.Unlock()
	if len(initialFinds) == 0 {
		log.Println("...No new items found on initial Funcorp check.")
		sendTelegramMessage(TELEGRAM_CHAT_ID, "✅ No new Funcorp listings found on initial check.")
	}
	for {
		mutex.Lock()
		interval := checkInterval
		paused := isPaused
		mutex.Unlock()
		select {
		case <-time.After(interval):
			if !paused {
				newlyFound := checkForNewItemsFuncorp()
				mutex.Lock()
				checkHistory = append(checkHistory, CheckResult{Timestamp: time.Now(), FoundProducts: funcorpProductsToGeneric(newlyFound)})
				if len(checkHistory) > 10 {
					checkHistory = checkHistory[1:]
				}
				isMuted := heartbeatMuted
				currentInterval := checkInterval
				mutex.Unlock()

				if len(newlyFound) == 0 {
					log.Println("...No new items found.")
					if !isMuted {
						sendTelegramMessage(TELEGRAM_CHAT_ID, fmt.Sprintf("✅ No new listings found. Next check in ~%.0f seconds.", currentInterval.Seconds()))
					}
				}
			} else {
				log.Println("...Scraper paused.")
			}
		case <-stop:
			log.Println("Scraper worker shutting down.")
			return
		}
	}
}

// convert FuncorpProduct slice to generic Product slice used by CheckResult (for /recent)
func funcorpProductsToGeneric(fp []FuncorpProduct) []FuncorpProduct {
	return fp
}

// simple html escape for text inside HTML message (keeps it safe)
func htmlEscape(s string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(s)
}

// ---------------- Command listener (Telegram) ----------------
func commandListenerWorker(stop chan struct{}) {
	log.Println("🤖 Funcorp Command listener started.")
	var lastUpdateID int
	for {
		apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates?offset=%d&timeout=10", TELEGRAM_BOT_TOKEN, lastUpdateID+1)
		resp, err := http.Get(apiURL)
		if err != nil {
			log.Printf("Error getting updates: %v", err)
			time.Sleep(5 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		var updates TelegramUpdateResponse
		_ = json.Unmarshal(body, &updates)
		for _, update := range updates.Result {
			lastUpdateID = update.UpdateID
			if update.Message.Text == "" || update.Message.Chat.ID == 0 {
				continue
			}
			chatIDStr := strconv.FormatInt(update.Message.Chat.ID, 10)

			// Allow commands from Admin ID OR the Alert Channel ID
			if chatIDStr != ADMIN_CHAT_ID && chatIDStr != TELEGRAM_CHAT_ID {
				sendTelegramMessage(chatIDStr, "Sorry, you are not authorized.")
				continue
			}
			parts := strings.Fields(update.Message.Text)
			command := parts[0]
			switch command {
			case "/start":
				mutex.Lock()
				isPaused = false
				mutex.Unlock()
				sendTelegramMessage(chatIDStr, "▶️ Bot resumed.")
			case "/pause":
				mutex.Lock()
				isPaused = true
				mutex.Unlock()
				sendTelegramMessage(chatIDStr, "⏸️ Bot paused.")
			case "/stop":
				sendTelegramMessage(chatIDStr, "🛑 Stopping bot...")
				close(stop)
				return
			case "/mute":
				mutex.Lock()
				heartbeatMuted = true
				mutex.Unlock()
				sendTelegramMessage(chatIDStr, "🔕 Heartbeat notifications muted.")
			case "/unmute":
				mutex.Lock()
				heartbeatMuted = false
				mutex.Unlock()
				sendTelegramMessage(chatIDStr, "🔔 Heartbeat notifications enabled.")
			case "/setinterval":
				if len(parts) > 1 {
					i, err := strconv.Atoi(parts[1])
					if err == nil && i >= 10 {
						mutex.Lock()
						checkInterval = time.Duration(i) * time.Second
						mutex.Unlock()
						sendTelegramMessage(chatIDStr, fmt.Sprintf("✅ Interval set to %d seconds.", i))
					} else {
						sendTelegramMessage(chatIDStr, "❌ Invalid interval. Minimum 10 seconds.")
					}
				} else {
					sendTelegramMessage(chatIDStr, "Usage: /setinterval <seconds>")
				}
			case "/status":
				mutex.Lock()
				status := "▶️ Running"
				if isPaused {
					status = "⏸️ Paused"
				}
				hbStatus := "🔔 Active"
				if heartbeatMuted {
					hbStatus = "🔕 Muted"
				}
				interval := checkInterval
				itemCount := len(seenItems)
				mutex.Unlock()
				sendTelegramMessage(chatIDStr, fmt.Sprintf("<b>Bot Status:</b>\n%s\nCheck Interval: %.0f seconds\nHeartbeat: %s\nTracked items: %d", status, interval.Seconds(), hbStatus, itemCount))
			case "/recent":
				var sb strings.Builder
				sb.WriteString("<b>🔎 Recent Finds (Last 10 Checks)</b>\n\n")
				mutex.Lock()
				totalFound := 0
				for i := len(checkHistory) - 1; i >= 0; i-- {
					result := checkHistory[i]
					if len(result.FoundProducts) > 0 {
						totalFound += len(result.FoundProducts)
						loc, _ := time.LoadLocation("Asia/Kolkata")
						sb.WriteString(fmt.Sprintf("<b><u>Found at %s:</u></b>\n", result.Timestamp.In(loc).Format("03:04 PM, Jan 02")))
						for _, p := range result.FoundProducts {
							sb.WriteString(fmt.Sprintf("- <a href='%s'>%s</a>\n", p.URL, p.Title))
						}
						sb.WriteString("\n")
					}
				}
				mutex.Unlock()
				if totalFound == 0 {
					sb.WriteString("No new products found in the last 10 checks.")
				}
				sendTelegramMessage(chatIDStr, sb.String())

			case "/dump":
				sendTelegramMessage(chatIDStr, "⏳ Starting full stock dump... This might take a moment.")
				go func() {
					allProds, err := getAllProductsScan()
					if err != nil {
						sendTelegramMessage(chatIDStr, fmt.Sprintf("❌ Error scanning: %v", err))
						return
					}

					count := 0
					for _, p := range allProds {
						if strings.EqualFold(p.StockStatus, "Sold out") || strings.Contains(strings.ToLower(p.StockStatus), "out of stock") {
							continue
						}

						// Send message for in-stock item
						message := fmt.Sprintf(
							"<b>📦 In Stock:</b>\n\n<b>%s</b>\n<b>Price:</b> %s\n<b>Stock:</b> %s\n\n<a href='%s'>View product</a>",
							htmlEscape(p.Title), htmlEscape(p.Price), htmlEscape(p.StockStatus), p.URL,
						)
						sendTelegramMessage(chatIDStr, message)
						count++
						// rate limit protection
						time.Sleep(2 * time.Second)
					}
					sendTelegramMessage(chatIDStr, fmt.Sprintf("✅ Dump complete. Sent %d in-stock items.", count))
				}()
			}
		}
	}
}

// ---------------- Keep-alive server ----------------
func startKeepAlive() {
	go func() {
		log.Println("⏰ Keep-alive will start in 10 seconds...")
		time.Sleep(10 * time.Second)
		ticker := time.NewTicker(8 * time.Minute)
		defer ticker.Stop()

		log.Printf("🔄 Keep-alive service started, pinging: %s", APP_KEEPALIVE_URL)
		client := &http.Client{Timeout: 15 * time.Second}

		// initial ping
		resp, err := client.Get(APP_KEEPALIVE_URL + "/ping")
		if err == nil {
			resp.Body.Close()
			log.Printf("✅ Initial keep-alive ping successful (status: %d)", resp.StatusCode)
		} else {
			log.Printf("⚠️ Initial ping failed: %v", err)
		}

		for range ticker.C {
			resp, err := client.Get(APP_KEEPALIVE_URL + "/ping")
			if err != nil {
				log.Printf("⚠️ Keep-alive ping failed: %v", err)
			} else {
				resp.Body.Close()
				log.Printf("✅ Keep-alive ping successful (status: %d)", resp.StatusCode)
			}
		}
	}()
}

// slight helper to convert FuncorpProduct slice into generic CheckResult.FoundProducts for storage/display
// (we already used FuncorpProduct as the FoundProducts type, so this is identity)
// but we keep the type same in CheckResult already
// ---------------- Main ----------------
func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("--- 🔥 Funcorp Hot Wheels Bot Starting ---")

	// basic HTTP server for health checks (Render)
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("🔥 Funcorp Hot Wheels Bot is running! 🔥"))
		})
		http.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			mutex.Lock()
			status := "running"
			if isPaused {
				status = "paused"
			}
			interval := checkInterval
			itemCount := len(seenItems)
			mutex.Unlock()
			response := fmt.Sprintf(`{
				"status": "%s",
				"check_interval_seconds": %.0f,
				"tracked_items": %d,
				"bot": "Funcorp Hot Wheels Bot",
				"timestamp": "%s"
			}`, status, interval.Seconds(), itemCount, time.Now().Format("2006-01-02 15:04:05"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(response))
		})
		http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("pong"))
		})
		log.Printf("🌐 HTTP server starting on port %s", port)
		if err := http.ListenAndServe(":"+port, nil); err != nil {
			log.Printf("❌ HTTP server error: %v", err)
		}
	}()

	// start keep-alive (pings your Render URL to keep app awake)
	startKeepAlive()

	// load or initialize baseline
	if _, err := os.Stat(SEEN_ITEMS_FILE_FUNC); os.IsNotExist(err) {
		initializeBaseline(SEEN_ITEMS_FILE_FUNC)
	}
	loadSeenItems(SEEN_ITEMS_FILE_FUNC)
	log.Printf("✅ Loaded baseline with %d items.", len(seenItems))

	stop := make(chan struct{})
	go scraperWorker(stop)
	go commandListenerWorker(stop)
	sendTelegramMessage(ADMIN_CHAT_ID, "🚀 Funcorp Hot Wheels Bot is online and running!")
	<-stop
	log.Println("--- Bot has been shut down. ---")
}




