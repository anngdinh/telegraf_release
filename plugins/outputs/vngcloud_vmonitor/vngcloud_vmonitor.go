package vngcloud_vmonitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/common/proxy"
	"github.com/influxdata/telegraf/plugins/inputs/system"
	"github.com/influxdata/telegraf/plugins/outputs"
	"github.com/influxdata/telegraf/plugins/serializers"
	"github.com/matishsiao/goInfo"
	"github.com/shirou/gopsutil/cpu"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

const (
	metricPath         = "/intake/v2/series"
	quotaPath          = "/intake/v2/check"
	defaultContentType = "application/json"
	agentVersion       = "1.26.0-2.0.0"
	retryTime          = 128 // = 2^7 => retry max 128*30s
)

var defaultConfig = &VNGCloudvMonitor{
	URL:             "http://localhost:8081",
	Timeout:         config.Duration(10 * time.Second),
	IamURL:          "https://hcm-3.console.vngcloud.vn/iam/accounts-api/v2/auth/token",
	checkQuotaRetry: config.Duration(30 * time.Second),
}

var sampleConfig = `
  ## URL is the address to send metrics to
  url = "http://localhost:8081"
  insecure_skip_verify = false
  data_format = "vngcloud_vmonitor"
  timeout = "30s"

  # From IAM service
  client_id = ""
  client_secret = ""
`

type Plugin struct {
	Name    string `json:"name"`
	Status  int    `json:"status"`
	Message string `json:"message"`
}

type QuotaInfo struct {
	Checksum string    `json:"checksum"`
	Data     *infoHost `json:"data"`
}

type infoHost struct {
	Plugins     []Plugin        `json:"plugins"`
	PluginsList map[string]bool `json:"-"`

	HashID string `json:"hash_id"`

	Kernel       string `json:"kernel"`
	Core         string `json:"core"`
	Platform     string `json:"platform"`
	OS           string `json:"os"`
	Hostname     string `json:"host_name"`
	CPUs         int    `json:"cpus"`
	ModelNameCPU string `json:"model_name_cpu"`
	Mem          uint64 `json:"mem"`
	IP           string `json:"ip"`
	AgentVersion string `json:"agent_version"`
	UserAgent    string `toml:"user_agent"`
}

type VNGCloudvMonitor struct {
	URL             string          `toml:"url"`
	Timeout         config.Duration `toml:"timeout"`
	ContentEncoding string          `toml:"content_encoding"`
	Insecure        bool            `toml:"insecure_skip_verify"`
	proxy.HTTPProxy
	Log telegraf.Logger `toml:"-"`

	IamURL       string `toml:"iam_url"`
	ClientID     string `toml:"client_id"`
	ClientSecret string `toml:"client_secret"`

	serializer serializers.Serializer
	infoHost   *infoHost
	clientIam  *http.Client

	checkQuotaRetry config.Duration
	dropCount       int
	dropTime        time.Time
	checkQuotaFirst bool
}

func (h *VNGCloudvMonitor) SetSerializer(serializer serializers.Serializer) {
	h.serializer = serializer
}

func (h *VNGCloudvMonitor) initHTTPClient() error {
	h.Log.Debug("Init client-iam ...")
	oauth2ClientConfig := &clientcredentials.Config{
		ClientID:     h.ClientID,
		ClientSecret: h.ClientSecret,
		TokenURL:     h.IamURL,
	}
	proxyFunc, err := h.Proxy()
	if err != nil {
		return err
	}
	ctx := context.WithValue(context.TODO(), oauth2.HTTPClient, &http.Client{
		Transport: &http.Transport{
			Proxy: proxyFunc,
		},
		Timeout: time.Duration(h.Timeout),
	})
	token, err := oauth2ClientConfig.TokenSource(ctx).Token()
	if err != nil {
		return fmt.Errorf("failed to get token: %s", err.Error())
	}

	_, err = json.Marshal(token)
	if err != nil {
		return fmt.Errorf("failed to Marshal token: %s", err.Error())
	}
	h.clientIam = oauth2ClientConfig.Client(ctx)
	h.Log.Info("Init client-iam successfully !")
	return nil
}

func (h *VNGCloudvMonitor) getIp(address, port string) (string, error) {
	h.Log.Infof("Dial %s %s", address, port)
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(address, port), 5*time.Second)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	return strings.Split(conn.LocalAddr().String(), ":")[0], nil
}

func getModelNameCPU() (string, error) {
	a, err := cpu.Info()
	if err != nil {
		return "", err
	}
	return a[0].ModelName, nil
}

func (h *VNGCloudvMonitor) getHostInfo() (*infoHost, error) {
	getHostPort := func(urlStr string) (string, error) {
		u, err := url.Parse(urlStr)
		if err != nil {
			return "", fmt.Errorf("url invalid %s", urlStr)
		}

		host, port, err := net.SplitHostPort(u.Host)

		if err != nil {
			return "", err
		}

		ipLocal, err := h.getIp(host, port)
		if err != nil {
			return "", err
		}
		return ipLocal, nil
	}

	var ipLocal string
	var err error
	// get ip local

	ipLocal, err = getHostPort(h.URL)

	if err != nil {
		return nil, fmt.Errorf("err getting ip address %s", err.Error())
	}
	// get ip local

	gi, err := goInfo.GetInfo()
	if err != nil {
		return nil, fmt.Errorf("error getting os info: %s", err)
	}
	ps := system.NewSystemPS()
	vm, err := ps.VMStat()

	if err != nil {
		return nil, fmt.Errorf("error getting virtual memory info: %s", err)
	}

	modelNameCPU, err := getModelNameCPU()

	if err != nil {
		return nil, fmt.Errorf("error getting cpu model name: %s", err)
	}

	h.infoHost = &infoHost{
		Plugins:      []Plugin{},
		PluginsList:  make(map[string]bool),
		Hostname:     "",
		HashID:       "",
		Kernel:       gi.Kernel,
		Core:         gi.Core,
		Platform:     gi.Platform,
		OS:           gi.OS,
		CPUs:         gi.CPUs,
		ModelNameCPU: modelNameCPU,
		Mem:          vm.Total,
		IP:           ipLocal,
		AgentVersion: agentVersion,
		UserAgent:    fmt.Sprintf("%s/%s (%s)", "vMonitorAgent", agentVersion, gi.OS),
	}
	h.setHostname(gi.Hostname)
	return h.infoHost, nil
}

func isUrl(str string) bool {
	u, err := url.Parse(str)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func (h *VNGCloudvMonitor) CheckConfig() error {
	ok := isUrl(h.URL)
	if !ok {
		return fmt.Errorf("URL Invalid %s", h.URL)
	}
	return nil
}

func (h *VNGCloudvMonitor) Connect() error {

	if err := h.CheckConfig(); err != nil {
		return err
	}

	// h.client_iam = client_iam
	err := h.initHTTPClient()
	if err != nil {
		h.Log.Info(err)
		return err
	}

	_, err = h.getHostInfo()
	if err != nil {
		return err
	}

	return nil
}

func (h *VNGCloudvMonitor) Close() error {
	return nil
}

func (h *VNGCloudvMonitor) Description() string {
	return "Configuration for vMonitor output."
}

func (h *VNGCloudvMonitor) SampleConfig() string {
	return sampleConfig
}

func (h *VNGCloudvMonitor) setHostname(hostname string) {
	hashCode := sha256.New()
	hashCode.Write([]byte(hostname))
	hashedID := hex.EncodeToString(hashCode.Sum(nil))

	h.infoHost.HashID = hashedID
	h.infoHost.Hostname = hostname
}

func (h *VNGCloudvMonitor) setPlugins(metrics []telegraf.Metric) error {
	hostname := ""
	for _, metric := range metrics {
		if _, exists := h.infoHost.PluginsList[metric.Name()]; !exists {
			hostTemp, ok := metric.GetTag("host")

			if ok {
				hostname = hostTemp
			}

			msg := "running"
			h.infoHost.Plugins = append(h.infoHost.Plugins, Plugin{
				Name:    metric.Name(),
				Status:  0,
				Message: msg,
			})
			h.infoHost.PluginsList[metric.Name()] = true
		}
	}
	if hostname != "" {
		h.setHostname(hostname)
	} else if h.infoHost.Hostname == "" {
		hostnameTemp, err := os.Hostname()
		if err != nil {
			return err
		}
		h.setHostname(hostnameTemp)
	}
	return nil
}

func (h *VNGCloudvMonitor) Write(metrics []telegraf.Metric) error {
	if h.dropCount > 1 && time.Now().Before(h.dropTime) {
		h.Log.Warnf("Drop %d metrics. Send request again at %s", len(metrics), h.dropTime.Format("15:04:05"))
		return nil
	}

	if err := h.setPlugins(metrics); err != nil {
		return err
	}

	if h.checkQuotaFirst {
		if isDrop, err := h.checkQuota(); err != nil {
			if isDrop {
				h.Log.Warnf("Drop metrics because of %s", err.Error())
				return nil
			}
			return err
		}
	}

	reqBody, err := h.serializer.SerializeBatch(metrics)
	if err != nil {
		return err
	}

	if err := h.write(reqBody); err != nil {
		return err
	}

	return nil
}

func (h *VNGCloudvMonitor) write(reqBody []byte) error {

	var reqBodyBuffer io.Reader = bytes.NewBuffer(reqBody)

	var err error
	if h.ContentEncoding == "gzip" {
		rc := internal.CompressWithGzip(reqBodyBuffer)
		if err != nil {
			return err
		}
		defer rc.Close()
		reqBodyBuffer = rc
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s%s", h.URL, metricPath), reqBodyBuffer)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", defaultContentType)
	req.Header.Set("checksum", h.infoHost.HashID)
	req.Header.Set("User-Agent", h.infoHost.UserAgent)

	if h.ContentEncoding == "gzip" {
		req.Header.Set("Content-Encoding", "gzip")
	}

	resp, err := h.clientIam.Do(req)
	if err != nil {
		if er := h.initHTTPClient(); er != nil {
			h.Log.Warnf("Drop metrics because can't init IAM: %s", er.Error())
			return nil
		}
		return fmt.Errorf("IAM request fail: %s", err.Error())
	}
	defer resp.Body.Close()
	dataRsp, err := io.ReadAll(resp.Body)

	if err != nil {
		return err
	}

	h.Log.Infof("Request-ID: %s with body length %d byte and response body %s", resp.Header.Get("Api-Request-ID"), len(reqBody), dataRsp)

	if isDrop, err := h.handleResponse(resp.StatusCode, dataRsp); err != nil {
		if isDrop {
			h.Log.Warnf("Drop metrics because of %s", err.Error())
			return nil
		}
		return err
	}
	return nil
}

func (h *VNGCloudvMonitor) handleResponse(respCode int, dataRsp []byte) (bool, error) {

	switch respCode {
	case 201:
		return false, nil
	case 401:
		return true, fmt.Errorf("IAM Unauthorized. Please check your service account")
	case 403:
		return true, fmt.Errorf("IAM Forbidden. Please check your permission")
	case 428:
		if isDrop, err := h.checkQuota(); err != nil {
			return isDrop, fmt.Errorf("can not check quota: %s", err.Error())
		}
	case 409:
		h.doubleCheckTime()
		return true, fmt.Errorf("CONFLICT. Please check your quota again")
	}
	return false, fmt.Errorf("status Code: %d, message: %s", respCode, dataRsp)
}

func (h *VNGCloudvMonitor) checkQuota() (bool, error) {
	h.Log.Info("Start check quota .....")
	h.checkQuotaFirst = true

	quotaStruct := &QuotaInfo{
		Checksum: h.infoHost.HashID,
		Data:     h.infoHost,
	}
	quotaJson, err := json.Marshal(quotaStruct)
	if err != nil {
		return false, fmt.Errorf("can not marshal quota struct: %s", err.Error())
	}

	req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s%s", h.URL, quotaPath), bytes.NewBuffer(quotaJson))
	if err != nil {
		return false, fmt.Errorf("error create new request: %s", err.Error())
	}
	req.Header.Set("checksum", h.infoHost.HashID)
	req.Header.Set("Content-Type", defaultContentType)
	req.Header.Set("User-Agent", h.infoHost.UserAgent)
	resp, err := h.clientIam.Do(req)

	if err != nil {
		return false, fmt.Errorf("send request checking quota failed: (%s)", err.Error())
	}
	defer resp.Body.Close()
	dataRsp, err := io.ReadAll(resp.Body)

	if err != nil {
		return false, fmt.Errorf("error occurred when reading response body: (%s)", err.Error())
	}

	isDrop := false
	// handle check quota
	switch resp.StatusCode {
	case 200:
		h.Log.Infof("Request-ID: %s. Checking quota success. Continue send metric.", resp.Header.Get("Api-Request-ID"))
		h.dropCount = 1
		h.dropTime = time.Now()
		h.checkQuotaFirst = false
		return false, nil

	case 401, 403:
		isDrop = true
	case 409:
		isDrop = true
		h.doubleCheckTime()
	}
	return isDrop, fmt.Errorf("Request-ID: %s. Checking quota fail (%d - %s)", resp.Header.Get("Api-Request-ID"), resp.StatusCode, dataRsp)
}

func init() {
	outputs.Add("vngcloud_vmonitor", func() telegraf.Output {
		infoHosts := &infoHost{
			// Plugins: map[string]*Plugin{
			// 	"haha": nil,
			// },
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
			Timeout:         defaultConfig.Timeout,
			URL:             defaultConfig.URL,
			IamURL:          defaultConfig.IamURL,
			checkQuotaRetry: defaultConfig.checkQuotaRetry,
			infoHost:        infoHosts,

			dropCount:       1,
			dropTime:        time.Now(),
			checkQuotaFirst: false,
		}
	})
}

func (h *VNGCloudvMonitor) doubleCheckTime() {
	if h.dropCount < retryTime {
		h.dropCount = h.dropCount * 2
	}
	h.dropTime = time.Now().Add(time.Duration(h.dropCount * int(time.Duration(h.checkQuotaRetry))))
}
