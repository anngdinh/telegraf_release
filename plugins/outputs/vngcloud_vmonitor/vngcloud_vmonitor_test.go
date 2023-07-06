package vngcloud_vmonitor

import (
	// "encoding/json"
	// "fmt"
	"fmt"
	// "strconv"
	"sync"
	"time"

	// "math"
	"net/http"
	"net/http/httptest"
	"net/url"

	// "reflect"
	"testing"
	// "time"

	"github.com/stretchr/testify/require"

	// "github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/serializers/vngcloud_vmonitor"
	"github.com/influxdata/telegraf/testutil"
)

func NewOutputPlugin(url, url2 string) *VNGCloudvMonitor {
	infoHosts := &infoHost{
		Plugins:     []Plugin{},
		PluginsList: make(map[string]bool),
		HashID:      "",
		Kernel:      "",
		Core:        "",
		Platform:    "",
		OS:          "",
		Hostname:    "",
		CPUs:        0,
		Mem:         0,
	}
	return &VNGCloudvMonitor{
		URL:             url,
		IamURL:          url2,
		Log:             testutil.Logger{},
		ClientId:        "fakeClient_id",
		ClientSecret:    "fakeClient_secret",
		checkQuotaRetry: config.Duration(1500 * time.Millisecond),

		Timeout:         defaultConfig.Timeout,
		infoHost:        infoHosts,
		dropCount:       1,
		dropTime:        time.Now(),
		checkQuotaFirst: false,
	}
}

func intakeTestServer3(code1, code2 int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		values := url.Values{}
		switch r.URL.Path {
		case quotaPath:
			w.WriteHeader(code1)
			values.Add("msg", fmt.Sprintf("%d", code1))
		case metricPath:
			w.WriteHeader(code2)
			values.Add("msg", fmt.Sprintf("%d", code2))
		}
		w.Write([]byte(values.Encode()))
	}
}

func iamTestServer3(code int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		switch code {
		case 200:
			w.WriteHeader(http.StatusOK)
			values := url.Values{}
			values.Add("access_token", "tokennnnnnnnnnnnnn")
			values.Add("tokenType", "Bearer")
			values.Add("expires_in", "5")
			w.Write([]byte(values.Encode()))
		default:
			w.WriteHeader(code)
			values := url.Values{}
			values.Add("msg", fmt.Sprintf("%d", code))
			w.Write([]byte(values.Encode()))
		}
	}
}

func TestDrop(t *testing.T) {
	intake_server := httptest.NewServer(http.NotFoundHandler())
	defer intake_server.Close()
	iammmm_server := httptest.NewServer(http.NotFoundHandler())
	defer iammmm_server.Close()

	intake_server.Config.Handler = http.HandlerFunc(intakeTestServer3(200, 201))
	iammmm_server.Config.Handler = http.HandlerFunc(iamTestServer3(200))

	tests := []struct {
		name    string
		codeArr [][]int // 0: quota, 1: metric, 2: iam, 3: expected error
	}{
		{
			name: "ok - disable SA - enable",
			codeArr: [][]int{
				{200, 201, 200, 0}, {200, 201, 200, 0},
				{200, 500, 500, 0},
				{200, 500, 500, 0},
				{200, 500, 500, 0},
				{200, 201, 200, 0}, {200, 201, 200, 0},
			},
		},
		{
			name: "ok - detach Policy - attach",
			codeArr: [][]int{
				{200, 201, 200, 0}, {200, 201, 200, 0},
				{200, 403, 200, 0},
				{200, 403, 200, 0},
				{200, 403, 200, 0},
				{200, 201, 200, 0}, {200, 201, 200, 0},
			},
		},

		{
			name: "ok - conflict quota - 1",
			codeArr: [][]int{
				{200, 201, 200, 0}, {200, 201, 200, 0},
				{200, 409, 200, 0},
				{200, 409, 200, 0},
				{200, 201, 200, 0}, {200, 201, 200, 0},
			},
		},
		{
			name: "ok - conflict quota - 2",
			codeArr: [][]int{
				{200, 201, 200, 0}, {200, 201, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{409, 409, 200, 0},
				{200, 201, 200, 0}, {200, 201, 200, 0},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fmt.Printf("\n\n%s\n", tt.name)
			fmt.Println("------------------------------------")
			plug := NewOutputPlugin(intake_server.URL, iammmm_server.URL)
			err := plug.Connect()
			require.NoError(t, err)
			serializer, _ := vngcloud_vmonitor.NewSerializer(time.Duration(10) * time.Second)
			plug.SetSerializer(serializer)

			var wg sync.WaitGroup
			var eRR error
			var quit = make(chan struct{})

			wg.Add(1)
			go routine(plug, quit, &wg, &eRR)
			time.Sleep(1 * time.Second)

			require.NoError(t, eRR)

			for i := 0; i < len(tt.codeArr); i++ {
				// each test
				intake_server.Config.Handler = http.HandlerFunc(intakeTestServer3(tt.codeArr[i][0], tt.codeArr[i][1]))
				iammmm_server.Config.Handler = http.HandlerFunc(iamTestServer3(tt.codeArr[i][2]))
				time.Sleep(2 * time.Second)
				fmt.Println()
				if tt.codeArr[i][3] == 0 {
					require.NoError(t, eRR)
				} else {
					require.Error(t, eRR)
				}
			}

			close(quit)
			wg.Wait()
			fmt.Println("done")
		})
	}
}

func routine(h *VNGCloudvMonitor, quit chan struct{}, wg *sync.WaitGroup, eRR *error) {
	for {
		select {
		case <-quit:
			wg.Done()
			return
		default:
			*eRR = work(h)
		}
	}
}
func work(h *VNGCloudvMonitor) error {
	time.Sleep(2 * time.Second)
	return h.Write(testutil.MockMetrics())
}
