package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type proxyProvider struct {
	validPeriod uint
	apiClt      *http.Client
	resChan     chan []string
	refreshFunc func(*http.Client) []string
}

func (p *proxyProvider) configure(cfg *config) error {
	p.apiClt = &http.Client{}
	p.resChan = make(chan []string)
	cfg.PredefinedProxies = sanitizePredifinedList(cfg.PredefinedProxies)
	if len(cfg.PredefinedProxies) > 0 {
		p.refreshFunc = func(_ *http.Client) []string {
			return cfg.PredefinedProxies
		}
		return nil
	}
	if cfg.ProviderAPIURLTemplate == "" {
		return errors.New("Invalid config")
	}
	p.validPeriod = cfg.ValidPeriod
	p.refreshFunc = func(with *http.Client) []string {
		return requestProxiesList(with, truncSprintf(cfg.ProviderAPIURLTemplate, cfg.Type, cfg.ExcludeCountries))
	}
	return nil
}

func (p *proxyProvider) next() string {
	pxy := <-p.resChan
	pxyLen := len(pxy)
	if pxyLen > 1 {
		p.resChan <- pxy[:pxyLen-1]
		return pxy[pxyLen-1]
	}
	p.resChan <- []string{}
	if pxyLen == 0 {
		return ""
	}
	return pxy[0]
}

func (p *proxyProvider) handleExhaustment() {
	lastUpdated := time.Now()
	for {
		pxy := <-p.resChan
		if p.validPeriod > 0 && time.Now().After(lastUpdated.Add(time.Duration(p.validPeriod)*time.Minute)) {
			pxy = pxy[:0]
		}
		if len(pxy) <= 1 {
			pxy = append(p.refreshFunc(p.apiClt), pxy...)
		}
		p.resChan <- pxy
		lastUpdated = time.Now()
	}
}

type config struct {
	// ListenAddr is ip and port to listen on.
	ListenAddr string
	// PredefinedProxies is the list of proxy addresses.
	// Should be in the following format: scheme://ip:port
	// If the list isn't empty only PredefinedProxies would be used
	// and all the following settings would be ignored.
	// If the list isn't empty byt no valid looking addresses found following settings would be tried anyway.
	PredefinedProxies []string
	// Type accepts "socks5", "http" and "https".
	// If empty defaults to "socks5".
	Type string
	// ValidPeriod set in minutes.
	// Tells how long to use acquired proxies before rotate.
	// If ValidPeriod is 0 proxyes list would be refreshed only after all the proxies are served to this service's client(s).
	ValidPeriod uint
	// ExcludeCountries is the string of comma separated ISO 3166-1 alpha-2 country codes.
	ExcludeCountries string
	// ProviderAPIURLTemplate is the string template to fill with proxy type and country exclusions.
	// It would be just fmt.Sprintf'ed.
	// Some examples:
	// "http://pubproxy.com/api/proxy?type=%s&not_country=%s&post=true&https=true"
	// "https://api.getproxylist.com/proxy?protocol=%s&notCountry=%s&allowsPost=true&allowsHttps=true"
	// "https://gimmeproxy.com/api/getProxy?post=true&supportsHttps=true&protocol=%s&notCountry=%s"
	ProviderAPIURLTemplate string `toml:"urltemplate"`
	// Method is http method to use calling proxy provider api. Defaults to "GET".
	Method string
}

func main() {
	var err error
	var cfg config

	_, err = toml.DecodeFile("config.toml", &cfg)
	if err != nil {
		log.Fatal(err)
	}
	listenAddr := cfg.ListenAddr
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	provider, err := newProxyProvider(&cfg)
	if err != nil {
		log.Fatal(err)
	}

	http.Serve(
		listener,
		http.HandlerFunc(func(res http.ResponseWriter, req *http.Request) {
			purl, err := validProxyURL(provider.next(), cfg.Type)
			if err != nil {
				http.Error(res, err.Error(), http.StatusInternalServerError)
			}
			res.Write([]byte(purl))
		}),
	)
}

func newProxyProvider(cfg *config) (*proxyProvider, error) {
	p := proxyProvider{}
	// reverse for natural order on pop
	for i, j := 0, len(cfg.PredefinedProxies)-1; i < j; i, j = i+1, j-1 {
		cfg.PredefinedProxies[i], cfg.PredefinedProxies[j] = cfg.PredefinedProxies[j], cfg.PredefinedProxies[i]
	}
	err := p.configure(cfg)
	if err != nil {
		return nil, err
	}
	go p.handleExhaustment()
	p.resChan <- []string{}
	return &p, nil
}

func requestProxiesList(with *http.Client, from string) []string {
	req, _ := http.NewRequest("GET", from, nil)
	res, _ := with.Do(req)
	defer res.Body.Close()
	bbyt, _ := ioutil.ReadAll(res.Body)
	return parseServerAndHostFromRes(bbyt)
}

func validProxyURL(addr string, fallbkScm string) (string, error) {
	addr = strings.Trim(addr, "\n\t\r 	")
	if strings.HasPrefix(addr, "socks5://") == false &&
		strings.HasPrefix(addr, "https://") == false &&
		strings.HasPrefix(addr, "http://") == false {
		if fallbkScm == "" {
			fallbkScm = "socks5"
		}
		addr = fmt.Sprintf("%s://%s", fallbkScm, addr)
	}
	u, err := url.Parse(addr)
	if err != nil {
		return "", err
	}
	return u.String(), err
}

func sanitizePredifinedList(pxy []string) []string {
	cleanList := []string{}
	for _, p := range pxy {
		u, err := validProxyURL(p, "")
		if err == nil {
			cleanList = append(cleanList, u)
		}
	}
	return cleanList
}

func parseServerAndHostFromRes(body []byte) []string {
	var outerMapOfRaws map[string]json.RawMessage
	if err := json.Unmarshal(body, &outerMapOfRaws); err != nil {
		return splitTextData(body)
	}
	return digInJSONRaw(outerMapOfRaws)
}

// Superugly as hell function, but it works.
func digInJSONRaw(data map[string]json.RawMessage) []string {
	result := []string{}
	var ip string
	var port string
	var mapOfRaws map[string]json.RawMessage
	var listOfRaws []json.RawMessage
	for k, v := range data {
		if k == "ip" {
			json.Unmarshal(v, &ip)
			ip = ip + ":"
			if ip != "" && port != "" {
				result = append(result, ip+port)
				ip, port = "", ""
			}
		} else if k == "port" {
			var p json.Number
			json.Unmarshal(v, &p)
			port = p.String()
			if ip != "" && port != "" {
				result = append(result, ip+port)
				ip, port = "", ""
			}
		} else {
			err := json.Unmarshal(v, &mapOfRaws)
			if err != nil {
				err = json.Unmarshal(v, &listOfRaws)
				if ip+port == "" && len(listOfRaws) > 0 {
					json.Unmarshal(listOfRaws[0], &mapOfRaws)
					result = append(result, digInJSONRaw(mapOfRaws)...)
				}
			}
			if ip+port == "" {
				result = append(result, digInJSONRaw(mapOfRaws)...)
			}
		}
	}

	return result
}

func splitTextData(data []byte) []string {
	return strings.Split(strings.ReplaceAll(strings.Trim(string(data), "\n\t "), ",", "\n"), "\n")
}

func truncSprintf(s string, args ...interface{}) string {
	n := strings.Count(s, `%s`)
	if n > len(args) {
		return fmt.Sprintf(s, args...)
	}
	return fmt.Sprintf(s, args[:n]...)
}
