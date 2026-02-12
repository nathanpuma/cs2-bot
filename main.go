package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

var ctx = context.Background()
var rdb *redis.Client

var (
	BUFF_COOKIE     string
	GAMERPAY_TOKEN  string
	CSFLOAT_API_KEY string
	PROXY_URL       string
)

const GAMERPAY_DISPLAY_MULTIPLIER = 1.196

var (
	cnyToUsdRate = 0.138
	eurToUsdRate = 1.08
	rateMutex    sync.RWMutex
)

var highPriorityCategories = []string{
	"weapon_ak47", "weapon_m4a1", "weapon_m4a1_silencer", "weapon_awp",
	"weapon_sport_gloves", "weapon_driver_gloves", "weapon_moto_gloves",
	"weapon_specialist_gloves", "weapon_hand_wraps",
	"weapon_knife_karambit", "weapon_knife_m9_bayonet", "weapon_knife_butterfly", "weapon_bayonet",
	"weapon_knife_flip", "weapon_knife_gut", "weapon_knife_tactical", "weapon_knife_falchion",
	"weapon_knife_survival_bowie", "weapon_knife_stiletto", "weapon_knife_ursus", "weapon_knife_skeleton",
	"weapon_knife_outdoor", "weapon_knife_canis", "weapon_knife_cord", "weapon_knife_css",
	"weapon_knife_gypsy_jackknife", "weapon_knife_widowmaker", "weapon_knife_push",
}

type Opportunity struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	GeneratedAt string  `json:"generated_at"`
	Route       Route   `json:"route"`
	Links       Links   `json:"links"`
	Note        string  `json:"note"`
}

type Route struct {
	From             string  `json:"from"`
	To               string  `json:"to"`
	BuyPrice         float64 `json:"buy_price"`
	BuyQuantity      int     `json:"buy_quantity"`
	SellListingPrice float64 `json:"sell_listing_price"`
	SellNetPrice     float64 `json:"sell_net_price"`
	SellQuantity     int     `json:"sell_quantity"`
	NetProfit        float64 `json:"net_profit"`
	ROI              float64 `json:"roi"`
	RiskLabel        string  `json:"risk_label"`
	LiquidityScore   string  `json:"liquidity_score"`
}

type Links struct {
	BuyURL  string `json:"buy_url"`
	SellURL string `json:"sell_url"`
}

type BuffResponse struct {
	Code string `json:"code"`
	Data struct {
		Items []struct {
			Name      string `json:"market_hash_name"`
			SellPrice string `json:"sell_min_price"`
			GoodsID   int    `json:"id"`
			SellNum   int    `json:"sell_num"`
		} `json:"items"`
	} `json:"data"`
	Msg string `json:"msg"`
}

type DMarketAggResponse struct {
	AggregatedTitles []struct {
		MarketHashName string `json:"MarketHashName"`
		Offers         struct {
			BestPrice string `json:"BestPrice"`
			Count     int    `json:"Count"`
		} `json:"Offers"`
	} `json:"AggregatedTitles"`
}

type CSFloatItem struct {
	MarketHashName string  `json:"market_hash_name"`
	Qty            int     `json:"qty"`
	MinPrice       float64 `json:"min_price"`
}

type GamerPayJSONResponse struct {
	Items   []GamerPayJSONItem `json:"items"`
	HasMore bool               `json:"hasMore"`
}

type GamerPayJSONItem struct {
	ID       int     `json:"id"`
	Name     string  `json:"name"`
	Price    int     `json:"price"`
	Wear     float64 `json:"wear"`
	Tradable bool    `json:"tradable"`
}

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Println("No .env file found")
	}

	BUFF_COOKIE = os.Getenv("BUFF_COOKIE")
	GAMERPAY_TOKEN = os.Getenv("GAMERPAY_TOKEN")
	CSFLOAT_API_KEY = os.Getenv("CSFLOAT_API_KEY")
	PROXY_URL = os.Getenv("PROXY_URL")
}

func main() {
	rdb = redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if _, err := rdb.Ping(ctx).Result(); err != nil {
		log.Fatalf("❌ Redis died: %v", err)
	}
	fmt.Println("✅ Redis connected.")

	var wg sync.WaitGroup

	go func() {
		for {
			updateCurrencyRates()
			time.Sleep(1 * time.Hour)
		}
	}()

	go func() {
		for {
			fetchBuffWorker()
			fmt.Println("💤 Buff: Sleeping 90 seconds...")
			time.Sleep(90 * time.Second)
		}
	}()

	go func() {
		for {
			fetchGamerPay()
			fmt.Println("💤 GamerPay: Sleeping 10 mins...")
			time.Sleep(10 * time.Minute)
		}
	}()

	go func() {
		fmt.Println("⏳ DMarket Worker Initialized (Starting in 5s)...")
		time.Sleep(5 * time.Second)
		for {
			fetchDMarketBatch()
			fmt.Println("💤 DMarket: Sleeping 15 mins (Optimized)...")
			time.Sleep(15 * time.Minute)
		}
	}()

	go func() {
		for {
			fetchCSFloatData()
			time.Sleep(10 * time.Minute)
		}
	}()

	go func() {
		for {
			fetchSkinportRobust()
			time.Sleep(10 * time.Minute)
		}
	}()

	http.HandleFunc("/api/opportunities", handleOpportunities)

	fmt.Println("🚀 FlipFinder API running at http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))

	wg.Wait()
}

func updateCurrencyRates() {
	fmt.Println("💱 Fetching Live Currency Rates...")

	resp, err := http.Get("https://api.frankfurter.app/latest?from=EUR&to=USD")
	if err == nil {
		defer resp.Body.Close()
		var res struct {
			Rates struct {
				USD float64 `json:"USD"`
			} `json:"rates"`
		}
		if json.NewDecoder(resp.Body).Decode(&res) == nil && res.Rates.USD > 0 {
			rateMutex.Lock()
			eurToUsdRate = res.Rates.USD
			rateMutex.Unlock()
			fmt.Printf("   ✅ EUR -> USD: %.4f\n", res.Rates.USD)
		}
	}

	respCNY, errCNY := http.Get("https://api.frankfurter.app/latest?from=CNY&to=USD")
	if errCNY == nil {
		defer respCNY.Body.Close()
		var resCNY struct {
			Rates struct {
				USD float64 `json:"USD"`
			} `json:"rates"`
		}
		if json.NewDecoder(respCNY.Body).Decode(&resCNY) == nil && resCNY.Rates.USD > 0 {
			rateMutex.Lock()
			cnyToUsdRate = resCNY.Rates.USD
			rateMutex.Unlock()
			fmt.Printf("   ✅ CNY -> USD: %.4f\n", resCNY.Rates.USD)
		}
	}
}

func handleOpportunities(w http.ResponseWriter, r *http.Request) {
	minProfit, _ := strconv.ParseFloat(r.URL.Query().Get("min_profit"), 64)
	if minProfit == 0 {
		minProfit = 3.0
	}
	minROI, _ := strconv.ParseFloat(r.URL.Query().Get("min_roi"), 64)
	if minROI == 0 {
		minROI = 1.0
	}

	keys, err := rdb.Keys(ctx, "price:*").Result()
	if err != nil {
		http.Error(w, "Redis Error", 500)
		return
	}

	pipe := rdb.Pipeline()
	cmds := make(map[string]*redis.MapStringStringCmd)
	for _, key := range keys {
		cmds[key] = pipe.HGetAll(ctx, key)
	}
	pipe.Exec(ctx)

	var opportunities []Opportunity

	sellMultipliers := map[string]float64{
		"buff163":  0.975,
		"csfloat":  0.98,
		"skinport": 0.88,
		"dmarket":  0.93,
		"gamerpay": 0.975,
	}

	for key, cmd := range cmds {
		val, _ := cmd.Result()
		name := key[6:]

		prices := make(map[string]float64)
		quantities := make(map[string]int)
		sources := []string{"buff163", "csfloat", "skinport", "dmarket", "gamerpay"}

		for _, source := range sources {
			pStr := val[source+"_price"]
			qStr := val[source+"_qty"]
			if pStr != "" {
				pCents, _ := strconv.Atoi(pStr)
				prices[source] = float64(pCents) / 100.0
			}
			if qStr != "" {
				q, _ := strconv.Atoi(qStr)
				quantities[source] = q
			}
		}

		var bestBuySrc string
		bestBuyPrice := 999999.0

		for source, price := range prices {
			if price > 0 && price < bestBuyPrice {
				if quantities[source] > 0 || source == "dmarket" {
					bestBuyPrice = price
					bestBuySrc = source
				}
			}
		}
		if bestBuySrc == "" {
			continue
		}

		var bestSellSrc string
		bestSellNet := 0.0

		for source, price := range prices {
			if source == bestBuySrc {
				continue
			}
			if price <= 0 {
				continue
			}

			destQty := quantities[source]
			if destQty <= 5 {
				continue
			}

			multiplier := sellMultipliers[source]
			netIncome := price * multiplier

			if netIncome > bestSellNet {
				bestSellNet = netIncome
				bestSellSrc = source
			}
		}
		if bestSellSrc == "" {
			continue
		}

		profit := bestSellNet - bestBuyPrice
		roi := (profit / bestBuyPrice) * 100

		if profit < minProfit || roi < minROI {
			continue
		}

		buffQty := quantities["buff163"]
		riskLabel := "RISKY"
		liquidity := "Low"

		if buffQty > 100 {
			riskLabel = "SAFE"
			liquidity = "Very High"
		} else if buffQty > 20 {
			riskLabel = "MODERATE"
			liquidity = "Medium"
		} else if buffQty > 5 {
			riskLabel = "CAUTION"
			liquidity = "Low"
		}

		if riskLabel == "RISKY" && roi < 30 {
			continue
		}

		idRaw := fmt.Sprintf("%s-%s-%s-%.2f", name, bestBuySrc, bestSellSrc, bestBuyPrice)
		hash := md5.Sum([]byte(idRaw))
		uniqueID := hex.EncodeToString(hash[:])
		buffID := val["buff163_id"]
		skinportLink := val["skinport_link"]

		note := ""
		if bestBuySrc == "gamerpay" {
			note = "🇪🇺 Tip: Europeans can save ~10% by paying in EUR!"
		}

		opp := Opportunity{
			ID:          uniqueID,
			Name:        name,
			GeneratedAt: time.Now().Format(time.RFC3339),
			Note:        note,
			Route: Route{
				From:             prettyName(bestBuySrc),
				To:               prettyName(bestSellSrc),
				BuyPrice:         bestBuyPrice,
				BuyQuantity:      quantities[bestBuySrc],
				SellListingPrice: prices[bestSellSrc],
				SellNetPrice:     math.Round(bestSellNet*100) / 100,
				SellQuantity:     quantities[bestSellSrc],
				NetProfit:        math.Round(profit*100) / 100,
				ROI:              math.Round(roi*100) / 100,
				RiskLabel:        riskLabel,
				LiquidityScore:   liquidity,
			},
			Links: Links{
				BuyURL:  generateLink(bestBuySrc, name, buffID, skinportLink),
				SellURL: generateLink(bestSellSrc, name, buffID, skinportLink),
			},
		}
		opportunities = append(opportunities, opp)
	}

	sort.Slice(opportunities, func(i, j int) bool {
		return opportunities[i].Route.NetProfit > opportunities[j].Route.NetProfit
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(opportunities)
}

func fetchBuffWorker() {
	client := &http.Client{Timeout: 15 * time.Second}
	fmt.Println("🐂 Buff163: Starting Scan...")

	for _, cat := range highPriorityCategories {
		for page := 1; page <= 5; page++ {
			url := fmt.Sprintf("https://buff.163.com/api/market/goods?game=csgo&page_num=%d&category=%s", page, cat)
			req, _ := http.NewRequest("GET", url, nil)
			req.Header.Add("Cookie", BUFF_COOKIE)
			req.Header.Add("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0.0.0 Safari/537.36")

			resp, err := client.Do(req)
			if err != nil {
				fmt.Printf("❌ Buff Net Error: %v\n", err)
				break
			}

			if resp.StatusCode != 200 {
				fmt.Printf("🛑 Buff Blocked (%d). Checking next category.\n", resp.StatusCode)
				resp.Body.Close()
				break
			}

			var result BuffResponse
			json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()

			if result.Code != "OK" || len(result.Data.Items) == 0 {
				break
			}

			pipe := rdb.Pipeline()
			count := 0
			for _, item := range result.Data.Items {
				cnyPrice, _ := strconv.ParseFloat(item.SellPrice, 64)
				rateMutex.RLock()
				currentRate := cnyToUsdRate
				rateMutex.RUnlock()
				usdPrice := cnyPrice * currentRate
				priceCents := int(usdPrice * 100)

				key := "price:" + item.Name
				pipe.HSet(ctx, key, "buff163_price", priceCents)
				pipe.HSet(ctx, key, "buff163_qty", item.SellNum)
				pipe.HSet(ctx, key, "buff163_id", item.GoodsID)
				count++
			}
			pipe.Exec(ctx)
			fmt.Printf(" -> Buff Saved %d items (%s pg %d)\n", count, cat, page)

			time.Sleep(3 * time.Second)
		}
	}
	fmt.Println("Buff163 Scan Complete.")
}

func fetchGamerPay() {
	fmt.Println("GamerPay: Starting Feed...")

	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(60),
		tls_client.WithClientProfile(profiles.Chrome_124),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar),
		tls_client.WithProxyUrl(PROXY_URL),
	}
	client, _ := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)

	page := 1
	for page < 50 {
		apiURL := fmt.Sprintf("https://api.gamerpay.gg/feed?page=%d", page)
		req, _ := fhttp.NewRequest("GET", apiURL, nil)

		if GAMERPAY_TOKEN != "" {
			req.Header.Set("Authorization", GAMERPAY_TOKEN)
		}

		resp, err := client.Do(req)
		if err != nil {
			break
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			break
		}

		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		var feed GamerPayJSONResponse
		if err := json.Unmarshal(bodyBytes, &feed); err != nil {
			break
		}

		if len(feed.Items) == 0 {
			break
		}

		pipe := rdb.Pipeline()
		count := 0
		for _, item := range feed.Items {
			if item.Price <= 0 {
				continue
			}

			priceEUR := float64(item.Price) / 100.0
			priceUSD := priceEUR * GAMERPAY_DISPLAY_MULTIPLIER
			priceUSDCents := int(math.Round(priceUSD * 100))

			nameToUse := item.Name
			cleanName := normalizeName(nameToUse)
			if cleanName == "SKIP" {
				continue
			}

			key := "price:" + cleanName
			pipe.HSet(ctx, key, "gamerpay_price", priceUSDCents)
			pipe.HSet(ctx, key, "gamerpay_qty", 1)
			pipe.Expire(ctx, key, 60*time.Minute)
			count++
		}
		pipe.Exec(ctx)
		fmt.Printf(" -> GamerPay Page %d saved %d items.\n", page, count)

		if len(feed.Items) < 10 {
			break
		}
		page++
		time.Sleep(2 * time.Second)
	}
}

func fetchDMarketBatch() {
	fmt.Println("DMarket: Starting Scan...")
	proxyUrl, _ := url.Parse(PROXY_URL)
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = http.ProxyURL(proxyUrl)
	client := &http.Client{Timeout: 30 * time.Second, Transport: t}

	keys, _ := rdb.Keys(ctx, "price:*").Result()
	var names []string
	for _, key := range keys {
		cleanName := strings.ReplaceAll(key[6:], "StatTrak™", "StatTrak")
		names = append(names, cleanName)
	}

	batchSize := 50
	for i := 0; i < len(names); i += batchSize {
		end := i + batchSize
		if end > len(names) {
			end = len(names)
		}

		batch := names[i:end]

		var escapedNames []string
		for _, n := range batch {
			escapedNames = append(escapedNames, url.QueryEscape(n))
		}
		titlesParam := strings.Join(escapedNames, ",")

		apiURL := fmt.Sprintf("https://api.dmarket.com/price-aggregator/v1/aggregated-prices?Titles=%s&Limit=50&GameId=a8db", titlesParam)

		req, _ := http.NewRequest("GET", apiURL, nil)
		resp, err := client.Do(req)

		if err != nil {
			fmt.Println("❌ DMarket Net Error")
			continue
		}

		if resp.StatusCode == 429 {
			fmt.Println("⏳ DMarket 429 - Sleeping 10s...")
			time.Sleep(10 * time.Second)
			resp.Body.Close()
			continue
		}

		if resp.StatusCode != 200 {
			resp.Body.Close()
			continue
		}

		var res DMarketAggResponse
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		pipe := rdb.Pipeline()
		for _, item := range res.AggregatedTitles {
			cleanName := normalizeName(item.MarketHashName)
			if cleanName == "SKIP" {
				continue
			}
			if item.Offers.Count > 0 && item.Offers.BestPrice != "" {
				pFloat, _ := strconv.ParseFloat(item.Offers.BestPrice, 64)
				pCents := int(pFloat * 100)
				key := "price:" + item.MarketHashName
				pipe.HSet(ctx, key, "dmarket_price", pCents)
				pipe.HSet(ctx, key, "dmarket_qty", item.Offers.Count)
			}
		}
		pipe.Exec(ctx)

		fmt.Printf("   -> DMarket Batch %d-%d done.\n", i, end)
		time.Sleep(200 * time.Millisecond)
	}

	fmt.Println("✅ DMarket Scan Complete.")
}

func fetchCSFloatData() {
	fmt.Println("🪂 CSFloat: Starting Sniper Mode (Live Listings)...")

	client := &http.Client{Timeout: 15 * time.Second}

	type CSFloatResponse struct {
		Data []struct {
			ID    string `json:"id"`
			Price int    `json:"price"`
			Item  struct {
				MarketHashName string  `json:"market_hash_name"`
				FloatValue     float64 `json:"float_value"`
			} `json:"item"`
		} `json:"data"`
	}

	for {
		req, _ := http.NewRequest("GET", "https://csfloat.com/api/v1/listings?limit=50&sort_by=most_recent&min_price=200", nil)

		req.Header.Set("Authorization", CSFLOAT_API_KEY)
		req.Header.Set("User-Agent", "FlipFinder/1.0")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("❌ CSFloat Net Error: %v\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		if resp.StatusCode == 429 {
			fmt.Println("⏳ CSFloat 429 (Too Fast) - Sleeping 60s...")
			resp.Body.Close()
			time.Sleep(60 * time.Second)
			continue
		}

		if resp.StatusCode != 200 {
			fmt.Printf("❌ CSFloat Error: %d\n", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(30 * time.Second)
			continue
		}

		var result CSFloatResponse
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			fmt.Println("❌ CSFloat JSON Error:", err)
			resp.Body.Close()
			time.Sleep(30 * time.Second)
			continue
		}
		resp.Body.Close()

		if len(result.Data) == 0 {
			time.Sleep(30 * time.Second)
			continue
		}

		pipe := rdb.Pipeline()
		count := 0
		for _, list := range result.Data {
			originalName := list.Item.MarketHashName
			if originalName == "" {
				continue
			}

			cleanName := normalizeName(originalName)
			if cleanName == "SKIP" {

				continue
			}

			key := "price:" + cleanName

			pipe.HSet(ctx, key, "csfloat_price", list.Price)
			pipe.HSet(ctx, key, "csfloat_qty", 1)

			pipe.Expire(ctx, key, 60*time.Minute)
			count++
		}
		pipe.Exec(ctx)

		fmt.Printf(" -> CSFloat Sniped %d new items. Sleeping 30s...\n", count)

		time.Sleep(30 * time.Second)
	}
}

func fetchSkinportRobust() {
	fmt.Println("🌍 Skinport: Starting Scan (TLS Mode)...")

	jar := tls_client.NewCookieJar()
	options := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(120),
		tls_client.WithClientProfile(profiles.Chrome_124),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithCookieJar(jar),
	}
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), options...)
	if err != nil {
		fmt.Println("❌ Skinport Client Init Error:", err)
		return
	}

	params := url.Values{}
	params.Add("app_id", "730")
	params.Add("currency", "USD")
	params.Add("tradable", "1")

	apiURL := "https://api.skinport.com/v1/items?" + params.Encode()
	req, _ := fhttp.NewRequest("GET", apiURL, nil)

	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	resp, err := client.Do(req)
	if err != nil {
		fmt.Println("❌ Skinport Net Error:", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		fmt.Printf("🛑 Skinport Blocked: %d\n", resp.StatusCode)
		return
	}

	bodyBytes, _ := ioutil.ReadAll(resp.Body)

	type SkinportItem struct {
		MarketHashName string   `json:"market_hash_name"`
		MinPrice       *float64 `json:"min_price"`
		Quantity       int      `json:"quantity"`
		ItemPage       string   `json:"item_page"`
	}

	var items []SkinportItem
	if err := json.Unmarshal(bodyBytes, &items); err != nil {
		fmt.Println("❌ Skinport JSON Error:", err)
		return
	}

	pipe := rdb.Pipeline()
	count := 0
	for _, item := range items {
		if item.MinPrice == nil {
			continue
		}

		cleanName := normalizeName(item.MarketHashName)
		if cleanName == "SKIP" {
			continue
		}

		key := "price:" + cleanName
		priceCents := int(*item.MinPrice * 100)

		pipe.HSet(ctx, key, "skinport_price", priceCents)
		pipe.HSet(ctx, key, "skinport_qty", item.Quantity)

		pipe.HSet(ctx, key, "skinport_link", item.ItemPage)

		pipe.Expire(ctx, key, 4*time.Hour)
		count++
	}
	pipe.Exec(ctx)
	fmt.Printf("✅ Skinport: Saved %d items.\n", count)
}

func generateLink(source, itemName, buffID string, externalLink string) string {
	cleanName := strings.ReplaceAll(itemName, "StatTrak™", "StatTrak")
	encoded := url.QueryEscape(cleanName)

	switch source {
	case "buff163":
		if buffID != "" && buffID != "0" {
			return "https://buff.163.com/goods/" + buffID
		}
		return "https://buff.163.com/market/goods?game=csgo&search=" + encoded

	case "gamerpay":
		return "https://gamerpay.gg/?query=" + encoded + "&autocompleted=1"

	case "dmarket":
		return "https://dmarket.com/ingame-items/item-list/csgo-skins?title=" + encoded

	case "csfloat":
		return "https://csfloat.com/search?market_hash_name=" + encoded + "&sort_by=lowest_price"

	case "skinport":

		if externalLink != "" {
			return externalLink
		}
		return "https://skinport.com/market?search=" + encoded
	}

	return ""
}

func prettyName(s string) string {
	switch s {
	case "buff163":
		return "Buff163"
	case "csfloat":
		return "CSFloat"
	case "skinport":
		return "Skinport"
	case "dmarket":
		return "DMarket"
	case "gamerpay":
		return "GamerPay"
	}
	return s
}

func normalizeName(rawName string) string {
	if !strings.Contains(rawName, "Doppler") && !strings.Contains(rawName, "Gamma") && !strings.Contains(rawName, "Marble Fade") {
		return rawName
	}

	validPhases := []string{
		"(Phase 1)", "(Phase 2)", "(Phase 3)", "(Phase 4)",
		"(Ruby)", "(Sapphire)", "(Black Pearl)", "(Emerald)",
		"(Fire and Ice)",
	}

	for _, p := range validPhases {
		if strings.Contains(rawName, p) {
			return rawName
		}
	}

	return "SKIP"
}