package speedtest

import (
	"bytes"
	"context"
	"crypto/des"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocarina/gocsv"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/ztelliot/taierspeed-cli/defs"
	"github.com/ztelliot/taierspeed-cli/report"
)

const (
	apiBaseUrl           = `https://dlc.cnspeedtest.com:8043`
	apiPerceptionBaseUrl = `https://ux.caict.ac.cn`
	apiWirelessBaseUrl   = `https://gw2.gdspeed.cn:9090`
)

var DomainRe = regexp.MustCompile(`([a-zA-Z0-9][-a-zA-Z0-9]{0,62}\.)+([a-zA-Z][-a-zA-Z]{0,62})`)

//go:embed serverlist.json
var ServerListByte []byte

//go:embed province.csv
var ProvinceListByte []byte

type PingJob struct {
	Index  int
	Server defs.Server
}

type PingResult struct {
	Index int
	Ping  float64
}

func GetRandom(tok, pre string, l int) string {
	if tok == "" {
		tok = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	}
	bs := []byte(tok)
	var res []byte
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < l; i++ {
		res = append(res, bs[r.Intn(len(bs))])
	}
	return pre + string(res)
}

func Encrypt(src, key string) string {
	data := []byte(src)
	keyByte := []byte(key)
	block, err := des.NewCipher(keyByte)
	if err != nil {
		panic(err)
	}
	bs := block.BlockSize()
	data = PKCS5Padding(data, bs)
	if len(data)%bs != 0 {
		panic("Need a multiple of the blocksize")
	}
	out := make([]byte, len(data))
	dst := out
	for len(data) > 0 {
		block.Encrypt(dst, data[:bs])
		data = data[bs:]
		dst = dst[bs:]
	}
	return hex.EncodeToString(out)
}

func Decrypt(src, key string) []byte {
	data, err := hex.DecodeString(src)
	if err != nil {
		panic(err)
	}
	keyByte := []byte(key)
	block, err := des.NewCipher(keyByte)
	if err != nil {
		panic(err)
	}
	bs := block.BlockSize()
	if len(data)%bs != 0 {
		panic("crypto/cipher: input not full blocks")
	}
	out := make([]byte, len(data))
	dst := out
	for len(data) > 0 {
		block.Decrypt(dst, data[:bs])
		data = data[bs:]
		dst = dst[bs:]
	}
	return PKCS5UnPadding(out)
}

func PKCS5Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - len(ciphertext)%blockSize
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}

func PKCS5UnPadding(origData []byte) []byte {
	length := len(origData)
	unpadding := int(origData[length-1])
	return origData[:(length - unpadding)]
}

func Register() (string, error) {
	did := GetRandom("0123456789abcdef", "", 16)
	key := GetRandom("0123456789", "taier", 6)
	pl := Encrypt(fmt.Sprintf("{\"deviceId\": \"%s\"}", did), key[:8])
	uri := fmt.Sprintf("%s/screen/taier/app/equipment/info?deviceId=%s&key=%s&json=%s", apiPerceptionBaseUrl, did, key, pl)

	req, err := http.NewRequest(http.MethodPost, uri, nil)
	if err != nil {
		log.Debugf("Failed when creating HTTP request: %s", err)
		return "", err
	}
	req.Header.Set("User-Agent", defs.AndroidUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("Failed when making HTTP request: %s", err)
		return "", err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed when reading HTTP response: %s", err)
		return "", err
	}

	if len(b) <= 0 {
		return "", err
	}

	return did, nil
}

// SpeedTest is the actual main function that handles the speed test(s)
func SpeedTest(c *cli.Context) error {
	// check for suppressed output flags
	var silent bool
	if c.Bool(defs.OptionSimple) || c.Bool(defs.OptionJSON) || c.Bool(defs.OptionCSV) {
		log.SetLevel(log.WarnLevel)
		silent = true
	}

	// check for debug flag
	if c.Bool(defs.OptionDebug) {
		log.SetLevel(log.DebugLevel)
	}

	// print help
	if c.Bool(defs.OptionHelp) {
		return cli.ShowAppHelp(c)
	}

	// print version
	if c.Bool(defs.OptionVersion) {
		log.SetOutput(os.Stdout)
		log.Warnf("%s %s (built on %s)", defs.ProgName, defs.ProgVersion, defs.BuildDate)
		log.Warn("Powered by TaierSpeed")
		log.Warn("Project: https://github.com/ztelliot/taierspeed-cli")
		log.Warn("Forked: https://github.com/librespeed/speedtest-cli")
		return nil
	}

	if c.String(defs.OptionSource) != "" && c.String(defs.OptionInterface) != "" {
		return fmt.Errorf("incompatible options '%s' and '%s'", defs.OptionSource, defs.OptionInterface)
	}

	// set CSV delimiter
	gocsv.TagSeparator = c.String(defs.OptionCSVDelimiter)

	// if --csv-header is given, print the header and exit (same behavior speedtest-cli)
	if c.Bool(defs.OptionCSVHeader) {
		var rep []report.CSVReport
		b, _ := gocsv.MarshalBytes(&rep)
		os.Stdout.WriteString(string(b))
		return nil
	}

	if req := c.Int(defs.OptionConcurrent); req <= 0 {
		log.Errorf("Concurrent requests cannot be lower than 1: %d is given", req)
		return errors.New("invalid concurrent requests setting")
	}

	// HTTP requests timeout
	http.DefaultClient.Timeout = time.Duration(c.Int(defs.OptionTimeout)) * time.Second

	forceIPv4 := c.Bool(defs.OptionIPv4)
	forceIPv6 := c.Bool(defs.OptionIPv6)
	noICMP := c.Bool(defs.OptionNoICMP)

	// TODO: change transport here

	var ispInfo *defs.IPInfoResponse
	// load server list
	var servers []defs.Server
	var err error
	var provinces []defs.ProvinceInfo
	var provs []defs.ProvinceInfo

	if err := gocsv.UnmarshalBytes(ProvinceListByte, &provinces); err != nil {
		log.Error("Failed to load province info")
		return err
	}

	if !c.Bool(defs.OptionList) {
		ispInfo, _ = getIPInfo()
	}

	isCN := false
	if len(c.StringSlice(defs.OptionProvince)) > 0 {
		op := c.StringSlice(defs.OptionProvince)
		for _, p := range provinces {
			if contains(op, p.ID) || contains(op, p.Code) {
				provs = append(provs, p)
			}
		}
		if len(provs) == 0 {
			err = errors.New("specified province(s) not found")
			log.Errorf("Error when parsing server list: %s", err)
			return err
		}
	} else if !c.Bool(defs.OptionList) && len(c.StringSlice(defs.OptionServer)) <= 0 {
		if ispInfo != nil && ispInfo.Country == "中国" {
			isCN = true
			provs = append(provs, getProvInfo(provinces, ispInfo.Region))
		} else {
			provs = append(provs, getProvInfo(provinces, ""))
		}
	}

	hasGlobal, hasPerception, hasWireless := false, false, false
	if len(c.StringSlice(defs.OptionServer)) > 0 {
		for _, s := range c.StringSlice(defs.OptionServer) {
			if strings.HasPrefix(s, "P") {
				hasPerception = true
			} else if strings.HasPrefix(s, "W") {
				hasWireless = true
			} else {
				hasGlobal = true
			}
		}
	} else {
		hasGlobal, hasPerception, hasWireless = true, true, true
	}

	// fetch the server list JSON and parse it into the `servers` array
	log.Infof("Retrieving server list")

	var serversT []defs.ServerGlobal

	if !c.Bool(defs.OptionDisableTai) && !forceIPv6 && hasGlobal {
		if isCN {
			serversT, err = getGlobalServerList(ispInfo.IP)
			for _, s := range serversT {
				servers = append(servers, defs.Server{ID: s.ID, IP: s.IP, Port: s.Port, Name: s.Name, Province: s.Prov, City: s.City, ISP: s.ISP})
			}
		} else {
			if err := json.Unmarshal(ServerListByte, &serversT); err == nil {
				for _, s := range serversT {
					if len(provs) <= 0 || checkProv(provs, s.Prov) {
						servers = append(servers, defs.Server{ID: s.ID, IP: s.IP, Port: s.Port, Name: s.Name, Province: s.Prov, City: s.City, ISP: s.ISP})
					}
				}
				servers, err = preprocessServers(servers, c.StringSlice(defs.OptionExclude), c.StringSlice(defs.OptionServer), !c.Bool(defs.OptionList))
			}
		}
	}

	var serversP []defs.Server
	var serversPT []defs.Server

	if !c.Bool(defs.OptionDisablePet) && !(isCN && len(servers) > 0) && hasPerception {
		if len(provs) <= 0 {
			serversP, err = getPerceptionServerList(nil)
		} else {
			for _, s := range provs {
				serversPT, err = getPerceptionServerList(&s)
				serversP = append(serversP, serversPT...)
			}
		}
		serversP, err = preprocessServers(serversP, c.StringSlice(defs.OptionExclude), c.StringSlice(defs.OptionServer), !c.Bool(defs.OptionList))
		servers = append(servers, serversP...)
	}

	var serversW []defs.Server

	if !c.Bool(defs.OptionDisableWir) && !(isCN && len(servers) > 0) && hasWireless {
		if len(provs) <= 0 || checkProv(provs, "广东") {
			serversW, err = getWirelessServerList()
			serversW, err = preprocessServers(serversW, c.StringSlice(defs.OptionExclude), c.StringSlice(defs.OptionServer), !c.Bool(defs.OptionList))
			servers = append(servers, serversW...)
		}
	}

	if len(servers) == 0 {
		err = errors.New("specified server(s) not found")
	}

	if err != nil {
		log.Errorf("Error when parsing server list: %s", err)
		return err
	}

	// if --list is given, list all the servers fetched and exit
	if c.Bool(defs.OptionList) {
		for _, svr := range servers {
			switch svr.Type {
			case defs.Perception:
				fmt.Printf("P%d: %s (%s)\n", svr.ID, svr.Name, svr.City)
			case defs.WirelessSpeed:
				fmt.Printf("W%d: %s (%s)\n", svr.ID, svr.Name, svr.ShowCity())
			default:
				fmt.Printf("%d: %s (%s)\n", svr.ID, svr.Name, svr.ShowCity())
			}
		}
		return nil
	}

	var network string
	switch {
	case forceIPv4:
		network = "ip4"
	case forceIPv6:
		network = "ip6"
	default:
		network = "ip"
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()

	// bind to source IP address or interface if given, or if ipv4/ipv6 is forced
	if src, iface := c.String(defs.OptionSource), c.String(defs.OptionInterface); src != "" || iface != "" || forceIPv4 || forceIPv6 {
		var localTCPAddr *net.TCPAddr
		if src != "" {
			// first we parse the IP to see if it's valid
			addr, err := net.ResolveIPAddr(network, src)
			if err != nil {
				if strings.Contains(err.Error(), "no suitable address") {
					if forceIPv6 {
						log.Errorf("Address %s is not a valid IPv6 address", src)
					} else {
						log.Errorf("Address %s is not a valid IPv4 address", src)
					}
				} else {
					log.Errorf("Error parsing source IP: %s", err)
				}
				return err
			}

			log.Debugf("Using %s as source IP", src)
			localTCPAddr = &net.TCPAddr{IP: addr.IP}
		}

		var defaultDialer *net.Dialer
		var dialContext func(context.Context, string, string) (net.Conn, error)

		if iface != "" {
			defaultDialer = newInterfaceDialer(iface)
			noICMP = true
		} else {
			defaultDialer = &net.Dialer{
				Timeout:   30 * time.Second,
				KeepAlive: 30 * time.Second,
			}
		}

		if localTCPAddr != nil {
			defaultDialer.LocalAddr = localTCPAddr
		}

		switch {
		case forceIPv4:
			dialContext = func(ctx context.Context, network, address string) (conn net.Conn, err error) {
				return defaultDialer.DialContext(ctx, "tcp4", address)
			}
		case forceIPv6:
			dialContext = func(ctx context.Context, network, address string) (conn net.Conn, err error) {
				return defaultDialer.DialContext(ctx, "tcp6", address)
			}
		default:
			dialContext = defaultDialer.DialContext
		}

		// set default HTTP client's Transport to the one that binds the source address
		// this is modified from http.DefaultTransport
		transport.DialContext = dialContext
	}

	http.DefaultClient.Transport = transport

	// if --server is given, do speed tests with all of them
	if len(c.StringSlice(defs.OptionServer)) > 0 || len(servers) == 1 {
		return doSpeedTest(c, servers, network, silent, noICMP, ispInfo)
	} else {
		// else select the fastest server from the list
		log.Info("Selecting the fastest server based on ping")

		var wg sync.WaitGroup
		jobs := make(chan PingJob, len(servers))
		results := make(chan PingResult, len(servers))
		done := make(chan struct{})

		pingList := make(map[int]float64)

		// spawn 10 concurrent pingers
		for i := 0; i < 10; i++ {
			go pingWorker(jobs, results, &wg, c.String(defs.OptionSource), network, noICMP)
		}

		// send ping jobs to workers
		for idx, server := range servers {
			wg.Add(1)
			jobs <- PingJob{Index: idx, Server: server}
		}

		go func() {
			wg.Wait()
			close(done)
		}()

	Loop:
		for {
			select {
			case result := <-results:
				pingList[result.Index] = result.Ping
			case <-done:
				break Loop
			}
		}

		if len(pingList) == 0 {
			log.Fatal("No server is currently available, please try again later.")
		}

		// get the fastest server's index in the `servers` array
		var serverIdx int
		for idx, ping := range pingList {
			if ping > 0 && ping <= pingList[serverIdx] {
				serverIdx = idx
			}
		}

		// do speed test on the server
		return doSpeedTest(c, []defs.Server{servers[serverIdx]}, network, silent, noICMP, ispInfo)
	}
}

func pingWorker(jobs <-chan PingJob, results chan<- PingResult, wg *sync.WaitGroup, srcIp, network string, noICMP bool) {
	for {
		job := <-jobs
		server := job.Server

		// check the server is up by accessing the ping URL and checking its returned value == empty and status code == 200
		if server.IsUp(network) {
			// skip ICMP if option given
			server.NoICMP = noICMP

			// if server is up, get ping
			ping, _, err := server.ICMPPingAndJitter(1, srcIp, network)
			if err != nil {
				log.Debugf("Can't ping server %s (%s), skipping", server.Name, server.IP)
				wg.Done()
				return
			}
			// return result
			results <- PingResult{Index: job.Index, Ping: ping}
			wg.Done()
		} else {
			log.Debugf("Server %s (%s) doesn't seem to be up, skipping", server.Name, server.IP)
			wg.Done()
		}
	}
}

// getPerceptionServerList fetches the server JSON from perception
func getPerceptionServerList(prov *defs.ProvinceInfo) ([]defs.Server, error) {
	// getting the server list from remote
	var servers []defs.Server
	var uri string
	old := false

	if prov != nil {
		uri = fmt.Sprintf("%s/screen/taier/app/getSpeedServiceByUserId?deviceId=%s&lon=%s&lat=%s&userId=-10000&province=%s&operatorId=-1", apiPerceptionBaseUrl, defs.DeviceID, prov.Lon, prov.Lat, prov.Name)
	} else {
		uri = fmt.Sprintf("%s/screen/taier/ftp/encrypt/information?deviceId=%s", apiPerceptionBaseUrl, defs.DeviceID)
		old = true
	}
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	req.URL.RawQuery = req.URL.Query().Encode()
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defs.AndroidUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if old {
		var resO map[string]json.RawMessage
		var data []string
		if err := json.Unmarshal(b, &resO); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(resO["data"], &data); err != nil {
			return nil, err
		}
		if string(resO["code"]) == "\"200\"" && len(data) == 2 {
			var res map[string]json.RawMessage
			key := data[0]
			data := data[1]
			if err := json.Unmarshal(Decrypt(data, key[:8]), &res); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(res["ftplist"], &servers); err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New(string(resO["message"]))
		}
	} else {
		var res map[string]json.RawMessage
		if err := json.Unmarshal(b, &res); err != nil {
			return nil, err
		}
		if string(res["code"]) == "\"200\"" {
			if err := json.Unmarshal(res["data"], &servers); err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New(string(res["msg"]))
		}
	}

	var serversResolved []defs.Server
	for _, s := range servers {
		s.Type = defs.Perception
		if downloadUrl, err := url.Parse(s.DownloadURL); err == nil {
			host := downloadUrl.Hostname()
			s.URL = host
			if DomainRe.MatchString(host) {
				if records, err := net.LookupHost(host); err == nil {
					for _, i := range records {
						if strings.Contains(i, ":") {
							s.IPv6 = i
						} else {
							s.IP = i
						}
					}
				}
			}
		}
		serversResolved = append(serversResolved, s)
	}

	var serversFiltered []defs.Server
	if prov != nil {
		for _, s := range serversResolved {
			if checkProv([]defs.ProvinceInfo{*prov}, s.Province) {
				serversFiltered = append(serversFiltered, s)
			}
		}
		return serversFiltered, nil
	}

	return serversResolved, nil
}

func getGlobalServerList(ip string) ([]defs.ServerGlobal, error) {
	uri := fmt.Sprintf("%s/dataServer/mobilematch_list.php?ip=%s&network=4&ipv6=0", apiBaseUrl, ip)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defs.AndroidUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var s []defs.ServerGlobal
	if err := json.Unmarshal(b, &s); err == nil {
		return s, nil
	} else {
		return nil, err
	}
}

func getWirelessServerList() ([]defs.Server, error) {
	uri := fmt.Sprintf("%s/GSpeedTestCloud/test/broadbandAccessResource.action", apiWirelessBaseUrl)
	var data = defs.GDPayload{Nonce: GetRandom("", "", 16)}
	data.Init()

	payload, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, uri, bytes.NewBuffer(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("User-Agent", defs.BrowserUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var res map[string]json.RawMessage
	if err := json.Unmarshal(b, &res); err != nil {
		return nil, err
	}

	var servers []defs.Server
	type Data struct {
		Province  string
		Resources []defs.ServerWireless
	}
	var ret struct{ Datas []Data }
	if string(res["ret_code"]) == "0" {
		if err := json.Unmarshal(res["ret"], &ret); err != nil {
			return nil, err
		}
	} else {
		return nil, errors.New(string(res["ret_msg"]))
	}

	for _, d := range ret.Datas {
		for _, r := range d.Resources {
			servers = append(servers, defs.Server{ID: r.ID, Name: r.Name, IP: r.IP, IPv6: r.IPv6, URL: r.URL, URLv6: r.URLv6, Province: r.Prov, City: r.City, ISP: r.GetISP(), Type: defs.WirelessSpeed})
		}
	}

	return servers, nil
}

// preprocessServers makes some needed modifications to the servers fetched
func preprocessServers(servers []defs.Server, excludes, specific []string, filter bool) ([]defs.Server, error) {
	if len(excludes) > 0 && len(specific) > 0 {
		return nil, errors.New("either --exclude or --specific can be used")
	}

	if filter {
		// exclude servers from --exclude
		if len(excludes) > 0 {
			var ret []defs.Server
			for _, server := range servers {
				var s string
				switch server.Type {
				case defs.Perception:
					s = fmt.Sprintf("P%d", server.ID)
				case defs.WirelessSpeed:
					s = fmt.Sprintf("W%d", server.ID)
				default:
					s = strconv.Itoa(server.ID)
				}
				if contains(excludes, s) {
					continue
				}
				ret = append(ret, server)
			}
			return ret, nil
		}

		// use only servers from --server
		// special value -1 will test all servers
		if len(specific) > 0 && !contains(specific, "-1") {
			var ret []defs.Server
			for _, server := range servers {
				var s string
				switch server.Type {
				case defs.Perception:
					s = fmt.Sprintf("P%d", server.ID)
				case defs.WirelessSpeed:
					s = fmt.Sprintf("W%d", server.ID)
				default:
					s = strconv.Itoa(server.ID)
				}
				if contains(specific, s) {
					ret = append(ret, server)
				}
			}
			return ret, nil
		}
	}

	return servers, nil
}

func getProvInfo(provinces []defs.ProvinceInfo, name string) defs.ProvinceInfo {
	var prov defs.ProvinceInfo

	if name != "" {
		for _, p := range provinces {
			if p.Short == name || strings.Contains(p.Name, name) || strings.Contains(name, p.Short) {
				prov = p
				break
			}
		}
	}

	if prov.ID != "" {
		return prov
	} else {
		return defs.ProvinceInfo{ID: "31", Code: "sh", Short: "上海", Name: "上海市", Lon: "121.473667", Lat: "31.230525"}
	}
}

func getIPInfo() (*defs.IPInfoResponse, error) {
	var ipInfo defs.IPInfoResponse
	var ispRaw []string

	uri := fmt.Sprintf("%s/dataServer/getIpLocS.php", apiBaseUrl)
	req, err := http.NewRequest(http.MethodGet, uri, nil)
	if err != nil {
		log.Debugf("Failed when creating HTTP request: %s", err)
		return nil, err
	}
	req.Header.Set("User-Agent", defs.AndroidUA)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Debugf("Failed when making HTTP request: %s", err)
		return nil, err
	}
	defer resp.Body.Close()

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Debugf("Failed when reading HTTP response: %s", err)
		return nil, err
	}

	if len(b) > 0 {
		data := strings.Split(string(b), "|")
		ipInfo.IP = data[0]
		if err := json.Unmarshal([]byte(data[1]), &ispRaw); err == nil {
			ipInfo.Country = ispRaw[0]
			ipInfo.Region = ispRaw[1]
			ipInfo.City = ispRaw[2]
			ipInfo.District = ispRaw[3]
			ipInfo.Isp = ispRaw[4]
		}
	}

	return &ipInfo, nil
}

// contains is a helper function to check if a string is in a string array
func contains(arr []string, val string) bool {
	for _, v := range arr {
		if v == val {
			return true
		}
	}
	return false
}

func checkProv(arr []defs.ProvinceInfo, val string) bool {
	for _, v := range arr {
		if v.Short == val || v.Name == val || strings.Contains(val, v.Short) {
			return true
		}
	}
	return false
}
