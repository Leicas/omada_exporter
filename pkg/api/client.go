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
// endpoint first, and falls back to a split strategy for Omada v6.x where the
// Viewer role can only query wireless clients via the standard endpoint.
func (c *Client) getClientsWithFilters(filtersEnabled bool, mac string) ([]NetworkClient, error) {
	// Try the standard endpoint with no wireless filter (works on older controllers)
	clients, err := c.getClientsStandard(filtersEnabled, mac, "")
	if err == nil {
		return clients, nil
	}
	log.Debug().Err(err).Msg("Standard clients endpoint failed, trying wireless+wired split")

	// v6.x Viewer role workaround: wireless filter works, wired doesn't.
	// Fetch wireless clients via standard endpoint (full signal/RSSI/rate data),
	// then fetch wired clients from the insight fallback.
	var allClients []NetworkClient

	wireless, wirelessErr := c.getClientsStandard(filtersEnabled, mac, "true")
	if wirelessErr != nil {
		log.Debug().Err(wirelessErr).Msg("Wireless clients endpoint also failed, falling back to insight for all")
		// Full fallback to insight for everything
		allClients, err = c.getClientsFromInsight()
		if err != nil {
			return nil, fmt.Errorf("all client endpoints failed: standard (%v), insight (%v)", wirelessErr, err)
		}
	} else {
		allClients = append(allClients, wireless...)
		log.Info().Int("wireless", len(wireless)).Msg("Fetched wireless clients via standard endpoint")
	}

	// Fetch wired clients from insight fallback
	wired, wiredErr := c.getWiredClientsFromInsight()
	if wiredErr != nil {
		log.Debug().Err(wiredErr).Msg("Failed to get wired clients from insight")
	} else if wirelessErr == nil {
		// Only add wired if we got wireless from the standard endpoint
		// (otherwise getClientsFromInsight above already included both)
		allClients = append(allClients, wired...)
		log.Info().Int("wired", len(wired)).Msg("Fetched wired clients via insight fallback")
	}

	// If using switchMac filter, filter client-side
	if filtersEnabled && mac != "" {
		var filtered []NetworkClient
		for _, cl := range allClients {
			if cl.SwitchMac == mac {
				filtered = append(filtered, cl)
			}
		}
		return filtered, nil
	}

	return allClients, nil
}

func (c *Client) getClientsStandard(filtersEnabled bool, mac string, wirelessFilter string) ([]NetworkClient, error) {
	url := fmt.Sprintf("%s/%s/api/v2/sites/%s/clients", c.Config.Host, c.omadaCID, c.SiteId)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("currentPage", "1")
	q.Add("currentPageSize", "10000")
	q.Add("filters.active", "true")
	if wirelessFilter != "" {
		q.Add("filters.wireless", wirelessFilter)
	}
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
		Msg("Using insight/clients fallback (some metrics unavailable)")

	return clients, nil
}

// getWiredClientsFromInsight fetches only wired clients from the insight endpoint.
func (c *Client) getWiredClientsFromInsight() ([]NetworkClient, error) {
	allClients, err := c.getClientsFromInsight()
	if err != nil {
		return nil, err
	}
	var wired []NetworkClient
	for _, cl := range allClients {
		if !cl.Wireless {
			wired = append(wired, cl)
		}
	}
	return wired, nil
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
