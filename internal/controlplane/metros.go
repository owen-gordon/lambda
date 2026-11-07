package controlplane

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	cloudapi "unikraft.com/cloud/sdk/controlplane"
)

type metro struct {
	Code    string `json:"code"`
	Country string `json:"country,omitempty"`
}

func cloudMetros(token string) func(context.Context) ([]metro, error) {
	client := cloudapi.NewClient(cloudapi.WithToken(token))
	return func(ctx context.Context) ([]metro, error) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		response, err := client.ListMetros(ctx)
		if err != nil {
			return nil, err
		}
		if response == nil || response.Status != cloudapi.ResponseStatusSuccess || response.Data == nil {
			return nil, errors.New("metro discovery returned no data")
		}
		result := make([]metro, 0, len(response.Data.Metros))
		for _, item := range response.Data.Metros {
			if metroCode.MatchString(item.IataCode) {
				result = append(result, metro{Code: item.IataCode, Country: item.Country})
			}
		}
		if len(result) == 0 {
			return nil, errors.New("no metros available")
		}
		sort.Slice(result, func(i, j int) bool { return result[i].Code < result[j].Code })
		return result, nil
	}
}

func (c *controlPlane) availableMetros(ctx context.Context) ([]metro, error) {
	if c.listMetros != nil {
		return c.listMetros(ctx)
	}
	return []metro{{Code: c.metro}}, nil
}

func (c *controlPlane) getMetros(w http.ResponseWriter, r *http.Request) {
	metros, err := c.availableMetros(r.Context())
	if err != nil {
		apiError(w, 503, "cannot discover deployment metros; retry shortly")
		return
	}
	writeJSON(w, 200, struct {
		Default string  `json:"default"`
		Metros  []metro `json:"metros"`
	}{c.metro, metros})
}

func (c *controlPlane) selectMetro(w http.ResponseWriter, r *http.Request, spec *buildSpec) bool {
	if spec.Metro == "" {
		spec.Metro = c.metro
	}
	if spec.Metro == c.metro {
		return true
	}
	metros, err := c.availableMetros(r.Context())
	if err != nil {
		apiError(w, 503, "cannot validate deployment metro; retry shortly")
		return false
	}
	for _, metro := range metros {
		if metro.Code == spec.Metro {
			return true
		}
	}
	apiError(w, 400, "unknown deployment metro")
	return false
}

func (c *controlPlane) manager(metro string) instanceManager {
	if c.managerForMetro != nil {
		return c.managerForMetro(metro)
	}
	return c.instances
}
