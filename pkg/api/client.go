package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	log "github.com/rs/zerolog/log"
)

// gets clients by switch mac address
func (c *Client) GetClientByPort(switchMac string, port float64) (*NetworkClient, error) {
	clients, err := c.getClientsWithFilters(true, switchMac)
	if err != nil {
		return nil, err
	}
	for _, client := range clients {
		if client.Port == port {
			return &client, nil
		}
	}
	return nil, nil
}

// gets all clients
func (c *Client) GetClients() ([]NetworkClient, error) {
	client, err := c.getClientsWithFilters(false, "")
	if err != nil {
		return nil, err
	}

	return client, nil
}

// getClientsWithFilters fetches active clients. It tries the standard clients
// endpoint first, and falls back to the insight/clients endpoint for Omada
// controller v6.x where the standard endpoint may not be available for Viewer roles.
func (c *Client) getClientsWithFilters(filtersEnabled bool, mac string) ([]NetworkClient, error) {
	clients, err := c.getClientsStandard(filtersEnabled, mac)
	if err == nil {
		return clients, nil
	}
	log.Debug().Err(err).Msg("Standard clients endpoint failed, trying insight endpoint")

	// Fallback: use insight/clients endpoint (works on v6.x with Viewer role)
	clients, insightErr := c.getClientsFromInsight()
	if insightErr != nil {
		// Return the original error as it's more informative
		return nil, fmt.Errorf("clients endpoint failed: %w (insight fallback also failed: %v)", err, insightErr)
	}

	// If using insight with switchMac filter, filter client-side
	if filtersEnabled && mac != "" {
		var filtered []NetworkClient
		for _, cl := range clients {
			if cl.SwitchMac == mac {
				filtered = append(filtered, cl)
			}
		}
		return filtered, nil
	}

	return clients, nil
}

func (c *Client) getClientsStandard(filtersEnabled bool, mac string) ([]NetworkClient, error) {
	url := fmt.Sprintf("%s/%s/api/v2/sites/%s/clients", c.Config.Host, c.omadaCID, c.SiteId)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("currentPage", "1")
	q.Add("currentPageSize", "10000")
	q.Add("filters.active", "true")
	if filtersEnabled {
		q.Add("filters.switchMac", mac)
	}

	req.URL.RawQuery = q.Encode()

	resp, err := c.makeLoggedInRequest(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Debug().Bytes("data", body).Msg("Received data from clients endpoint")

	clients, err := parseListResult[NetworkClient](body)
	if err != nil {
		return nil, fmt.Errorf("failed to parse clients: %w", err)
	}

	return clients, nil
}

// getClientsFromInsight fetches clients from the insight endpoint and filters
// for recently active ones. This endpoint is available to Viewer roles on v6.x
// controllers where the standard clients endpoint returns "General error."
func (c *Client) getClientsFromInsight() ([]NetworkClient, error) {
	url := fmt.Sprintf("%s/%s/api/v2/sites/%s/insight/clients", c.Config.Host, c.omadaCID, c.SiteId)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("currentPage", "1")
	q.Add("currentPageSize", "10000")
	req.URL.RawQuery = q.Encode()

	resp, err := c.makeLoggedInRequest(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	log.Debug().Bytes("data", body).Msg("Received data from insight/clients endpoint")

	raw, err := checkResponse(body)
	if err != nil {
		return nil, err
	}

	type insightClient struct {
		Name     string  `json:"name"`
		Mac      string  `json:"mac"`
		Wireless bool    `json:"wireless"`
		Download float64 `json:"download"`
		Upload   float64 `json:"upload"`
		LastSeen int64   `json:"lastSeen"`
		VlanId   float64 `json:"vid"`
	}

	// Parse paginated result
	var paginated struct {
		Data []insightClient `json:"data"`
	}
	if err := json.Unmarshal(raw, &paginated); err != nil {
		return nil, fmt.Errorf("failed to parse insight clients: %w", err)
	}

	// Filter for active clients (seen in the last 10 minutes)
	cutoff := time.Now().Add(-10 * time.Minute).UnixMilli()
	var clients []NetworkClient
	for _, ic := range paginated.Data {
		if ic.LastSeen < cutoff {
			continue
		}
		clients = append(clients, NetworkClient{
			Name:        ic.Name,
			Mac:         ic.Mac,
			Wireless:    ic.Wireless,
			TrafficDown: ic.Download,
			TrafficUp:   ic.Upload,
			VlanId:      ic.VlanId,
		})
	}

	log.Info().Int("active", len(clients)).Int("total", len(paginated.Data)).
		Msg("Using insight/clients fallback (some wireless metrics unavailable)")

	return clients, nil
}
type NetworkClient struct {
	Name        string  `json:"name"`
	HostName    string  `json:"hostName"`
	Mac         string  `json:"mac"`
	Port        float64 `json:"port"`
	Ip          string  `json:"ip"`
	VlanId      float64 `json:"vid"`
	ApName      string  `json:"apName"`
	Wireless    bool    `json:"wireless"`
	SwitchMac   string  `json:"switchMac"`
	Vendor      string  `json:"vendor"`
	Activity    float64 `json:"activity"`
	SignalLevel float64 `json:"signalLevel"`
	SignalNoise float64 `json:"snr"`
	WifiMode    float64 `json:"wifiMode"`
	Ssid        string  `json:"ssid"`
	Rssi        float64 `json:"rssi"`
	TrafficDown float64 `json:"trafficDown"`
	TrafficUp   float64 `json:"trafficUp"`
	RxRate      float64 `json:"rxRate"`
	TxRate      float64 `json:"txRate"`
}
