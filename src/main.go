// cloudflare-prometheus-proxy.go (GraphQL version)
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/joho/godotenv"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Zone struct {
	Tag string
	ID  string
}

var (
	apiToken   = ""
	zones      = []Zone{}
	zonesMutex = &sync.RWMutex{}
	cfBase     = "https://api.cloudflare.com/client/v4"

	reqMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cloudflare_zone_requests_total",
			Help: "Total requests per zone (GraphQL 1dGroups API)",
		},
		[]string{"zone_tag", "date"},
	)

	cachedMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cloudflare_zone_cached_requests_total",
			Help: "Cached requests per zone (GraphQL 1dGroups API)",
		},
		[]string{"zone_tag", "date"},
	)

	byStatusMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "cloudflare_zone_status_code_requests_total",
			Help: "Requests per zone by HTTP status code",
		},
		[]string{"zone_tag", "date", "status_code"},
	)
)

func init() {
	prometheus.MustRegister(reqMetric)
	prometheus.MustRegister(cachedMetric)
	prometheus.MustRegister(byStatusMetric)
}

// func getZoneID(zoneTag string) (string, error) {
// 	req, _ := http.NewRequest("GET", cfBase+"/zones?name="+zoneTag, nil)
// 	req.Header.Set("Authorization", "Bearer "+apiToken)
// 	req.Header.Set("Content-Type", "application/json")

// 	resp, err := http.DefaultClient.Do(req)
// 	if err != nil {
// 		return "", err
// 	}
// 	defer resp.Body.Close()

// 	body, _ := io.ReadAll(resp.Body)
// 	var data struct {
// 		Result []struct {
// 			ID string `json:"id"`
// 		} `json:"result"`
// 	}
// 	if err := json.Unmarshal(body, &data); err != nil || len(data.Result) == 0 {
// 		return "", fmt.Errorf("failed to get zone ID for %s", zoneTag)
// 	}
// 	return data.Result[0].ID, nil
// }

func assignAllZones() error {
	u := fmt.Sprintf("%s/zones?per_page=500", cfBase)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var data struct {
		Result []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"result"`
		ResultInfo struct {
			Page       int `json:"page"`
			PerPage    int `json:"per_page"`
			TotalPages int `json:"total_pages"`
		} `json:"result_info"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &data); err != nil || len(data.Result) == 0 {
		return fmt.Errorf("failed to get all zones %s", err)
	}
	zonesCopy := []Zone{}
	for _, zone := range data.Result {
		if zone.Status == "active" {
			zoneCopy := Zone{
				Tag: zone.Name,
				ID:  zone.ID,
			}
			zonesCopy = append(zonesCopy, zoneCopy)
		}
	}
	if len(zonesCopy) == 0 {
		return fmt.Errorf("no active zones found")
	}
	log.Println("[OK] Found zones:", len(zonesCopy))

	zonesMutex.Lock()
	zones = zonesCopy
	zonesMutex.Unlock()

	return nil
}

func fetchZoneStats(zone Zone) {
	// zoneID, err := getZoneID(zoneTag)
	// if err != nil {
	// 	log.Printf("[!] Ошибка получения ID зоны %s: %v", zoneTag, err)
	// 	return
	// }
	log.Println("[OK] Loading zoneTag:zoneID", zone.Tag, ":", zone.ID)
	today := time.Now().AddDate(0, 0, -7).Format("2006-01-02")
	query := fmt.Sprintf(`{
		"query": "query { viewer { zones(filter: { zoneTag: \"%s\" }) { httpRequests1dGroups( filter: { date_geq: \"%s\" }, limit: 10, orderBy: [date_DESC]) { sum { requests cachedRequests responseStatusMap { edgeResponseStatus requests } } dimensions { date } } } } }"
	}`, zone.ID, today)

	req, _ := http.NewRequest("POST", "https://api.cloudflare.com/client/v4/graphql", bytes.NewBuffer([]byte(query)))
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[!] Ошибка Cloudflare GraphQL API для %s: %v", zone.Tag, err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Data struct {
			Viewer struct {
				Zones []struct {
					HttpRequests1dGroups []struct {
						Sum struct {
							Requests          float64 `json:"requests"`
							CachedRequests    float64 `json:"cachedRequests"`
							ResponseStatusMap []struct {
								EdgeResponseStatus json.Number `json:"edgeResponseStatus"`
								Requests           float64     `json:"requests"`
							} `json:"responseStatusMap"`
						} `json:"sum"`
						Dimensions struct {
							Date string `json:"date"`
						} `json:"dimensions"`
					} `json:"httpRequests1dGroups"`
				} `json:"zones"`
			} `json:"viewer"`
		} `json:"data"`
	}

	//log.Println("answer:", string(body))

	if err := json.Unmarshal(body, &result); err != nil {
		log.Printf("[!] Ошибка разбора GraphQL ответа для %s: %v", zone.Tag, err)
		return
	}

	for _, group := range result.Data.Viewer.Zones[0].HttpRequests1dGroups {
		reqMetric.WithLabelValues(zone.Tag, group.Dimensions.Date).Set(group.Sum.Requests)
		cachedMetric.WithLabelValues(zone.Tag, group.Dimensions.Date).Set(group.Sum.CachedRequests)
		for _, status := range group.Sum.ResponseStatusMap {
			EdgeResponseStatusStr := status.EdgeResponseStatus.String()
			if EdgeResponseStatusStr != "" {
				byStatusMetric.WithLabelValues(zone.Tag, group.Dimensions.Date, EdgeResponseStatusStr).Set(status.Requests)
			}
		}
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.Println("START")

	log.Println("runtime.GOMAXPROCS:", runtime.GOMAXPROCS(0))

	if err := godotenv.Load("../.env"); err != nil {
		log.Println("Cant load .env: ", err)
	}

	apiToken = os.Getenv("CLOUDFLARE_API_TOKEN")

	err := assignAllZones()
	if err != nil {
		log.Println("[!] Ошибка получения всех зон:", err)
		return
	}

	go func() {
		for {
			zonesMutex.RLock()
			for _, zone := range zones {
				fetchZoneStats(zone)
			}
			zonesMutex.RUnlock()

			time.Sleep(5 * time.Minute)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	log.Println("[OK] Слушаем :28191 /metrics")
	log.Fatal(http.ListenAndServe(":28191", nil))
}
