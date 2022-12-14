package cmd

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"golang.org/x/exp/slices"

	"github.com/shimmeris/SCFProxy/cmd/config"
	"github.com/shimmeris/SCFProxy/sdk"
	"github.com/shimmeris/SCFProxy/socks"
)

var deployCmd = &cobra.Command{
	Use:       "deploy [http|socks|reverse] -p providers -r regions",
	Short:     "Deploy module-specific proxies",
	ValidArgs: []string{"http", "socks", "reverse"},
	Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
	RunE: func(cmd *cobra.Command, args []string) error {
		providers, err := createProviders(cmd)
		if err != nil {
			return err
		}

		module := args[0]
		switch module {
		case "http":
			return deployHttp(providers)
		case "socks":
			addr, _ := cmd.Flags().GetString("addr")
			if addr == "" {
				return errors.New("missing parameter [-a/--addr]")
			}

			key, _ := cmd.Flags().GetString("key")
			if key == "" {
				return errors.New("missing parameter [-k/--key]")
			}
			if len(key) != socks.KeyLength {
				return errors.New(fmt.Sprintf("key must be %d bytes", socks.KeyLength))
			}
			if key == "random" {
				key = randomString(socks.KeyLength)
			}

			auth, _ := cmd.Flags().GetString("auth")
			return deploySocks(providers, addr, key, auth)
		case "reverse":
			origin, _ := cmd.Flags().GetString("origin")
			if origin == "" {
				return errors.New("missing parameter [-o/--origin]")
			}
			ips, _ := cmd.Flags().GetStringSlice("ip")
			return deployReverse(providers, origin, ips)
		}
		return nil
	},
}

func init() {
	rootCmd.AddCommand(deployCmd)
	deployCmd.Flags().StringSliceP("provider", "p", nil, "specify which cloud providers to deploy proxy")
	deployCmd.Flags().StringSliceP("region", "r", nil, "specify which regions of cloud providers deploy proxy")
	deployCmd.Flags().StringP("config", "c", config.ProviderConfigPath, "path of provider credential file")

	// deploy socks needed
	deployCmd.Flags().StringP("addr", "a", "", "[socks] host:port address of the cloud function callback")
	deployCmd.Flags().StringP("key", "k", "random", "[socks] 8-bytes string used to verify that the connection initiated to [-a host:port] is from the cloud function")
	deployCmd.Flags().String("auth", "", "[socks] username:password for socks proxy authentication")

	// deploy reverse needed
	deployCmd.Flags().StringP("origin", "o", "", "[reverse] Address of the reverse proxy back to the source")
	deployCmd.Flags().StringSlice("ip", nil, "[reverse] Restrict ips which can access the reverse proxy address")

	deployCmd.MarkFlagRequired("provider")
	deployCmd.MarkFlagRequired("region")
}

func createProviders(cmd *cobra.Command) ([]sdk.Provider, error) {
	providerConfigPath, _ := cmd.Flags().GetString("config")
	providerConfig, err := config.LoadProviderConfig(providerConfigPath)
	if err != nil {
		return nil, err
	}

	providerNames, _ := cmd.Flags().GetStringSlice("provider")
	regionPatterns, _ := cmd.Flags().GetStringSlice("region")
	var providers []sdk.Provider
	for _, p := range providerNames {
		if !slices.Contains(allProviders, p) {
			logrus.Errorf("%s is not a valid provider", p)
			continue
		}

		if !providerConfig.IsSet(p) {
			logrus.Warningf("%s's credential config not set, will ignore", p)
			continue
		}

		regions := parseRegionPatterns(p, regionPatterns)
		if len(regions) == 0 {
			logrus.Error("No region avalible, pleast use list cmd to ")
			continue
		}

		for _, r := range regions {
			provider, err := createProvider(p, r, providerConfig)
			if err != nil {
				logrus.Error(err)
				continue
			}
			providers = append(providers, provider)
		}
	}
	return providers, nil
}

func parseRegionPatterns(provider string, regionPatterns []string) []string {
	// patter support 4 styles
	// *, ap-*, us-3, us-north-1, ap-beijing
	var usableRegions []string
	regions := listRegions(provider)

	for _, pattern := range regionPatterns {
		if pattern == "*" {
			usableRegions = regions
			break
		}

		// parse specific region name like ap-hongkong-1, cn-hangzhou
		if slices.Contains(regions, pattern) {
			usableRegions = append(usableRegions, pattern)
			continue
		}

		// parse region name like us-3, ap-*
		patternPart := strings.Split(pattern, "-")
		if len(patternPart) != 2 {
			logrus.Debugf("%s doesn't have region %s", provider, pattern)
			continue
		}

		prefix := patternPart[0]
		num := patternPart[1]

		var matched []string
		for _, r := range regions {
			if strings.HasPrefix(r, prefix) {
				matched = append(matched, r)
			}
		}

		if num == "*" {
			usableRegions = append(usableRegions, matched...)
			continue
		}

		n, err := strconv.Atoi(num)
		// err exists when region like cn-hangzhou provided, but the provider doesn't  have cn-hangzhou
		if err != nil {
			logrus.Debugf("%s doesn't have region %s", provider, pattern)
			continue
		}

		if n > len(matched) {
			n = len(matched)
		}
		usableRegions = append(usableRegions, matched[:n]...)
	}

	return removeDuplicate(usableRegions)
}

func removeDuplicate(data []string) []string {
	var result []string
	m := map[string]struct{}{}

	for _, d := range data {
		if _, ok := m[d]; ok {
			continue
		}
		result = append(result, d)
		m[d] = struct{}{}
	}
	return result
}

// TODO: Find a better way to avoid the duplication in `deployXxx` and `clearXxx` function
func deployHttp(providers []sdk.Provider) error {
	hconf, err := config.LoadHttpConfig()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(len(providers))

	for _, p := range providers {
		go func(p sdk.Provider) {
			defer wg.Done()
			provider, region := p.Name(), p.Region()
			hp, ok := p.(sdk.HttpProxyProvider)
			if !ok {
				logrus.Errorf("Provider %s can't deploy http", p.Name())
				return
			}

			onlyTrigger := false
			if record, ok := hconf.Get(provider, region); ok {
				if record.Api != "" {
					logrus.Infof("%s %s has been deployed, pass", provider, region)
					return
				}
				onlyTrigger = true
			}

			opts := &sdk.HttpProxyOpts{
				FunctionName: HTTPFunctionName,
				TriggerName:  HTTPTriggerName,
				OnlyTrigger:  onlyTrigger,
			}
			r, err := hp.DeployHttpProxy(opts)
			if err != nil {
				logrus.Error(err)
				return
			}

			logrus.Printf("[success] http proxy deployed in %s.%s", provider, region)
			hconf.Set(r.Provider, r.Region, &config.HttpRecord{Api: r.API})
		}(p)
	}

	wg.Wait()
	return hconf.Save()

}

func deploySocks(providers []sdk.Provider, addr, key, auth string) error {
	sconf, err := config.LoadSocksConfig()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(len(providers))

	for _, p := range providers {
		go func(p sdk.Provider) {
			defer wg.Done()
			provider, region := p.Name(), p.Region()
			sp, ok := p.(sdk.SocksProxyProvider)
			if !ok {
				logrus.Errorf("Provider %s can't deploy socks", provider)
				return
			}

			onlyTrigger := false
			if record, ok := sconf.Get(provider, region); ok {
				if record.Key != "" {
					logrus.Infof("%s %s has already been deployed", provider, region)
					return
				}
				onlyTrigger = true
			}

			opts := &sdk.SocksProxyOpts{
				FunctionName: SocksFunctionName,
				TriggerName:  SocksTriggerName,
				OnlyTrigger:  onlyTrigger,
				Key:          key,
				Addr:         addr,
				Auth:         auth,
			}
			if err := sp.DeploySocksProxy(opts); err != nil {
				logrus.Error(err)
				return
			}

			logrus.Printf("[success] socks proxy deployed in %s.%s", provider, region)
			tcpAddr, _ := net.ResolveTCPAddr("tcp", addr)
			sconf.Set(sp.Name(), sp.Region(), &config.SocksRecord{Key: key, Host: tcpAddr.IP.String(), Port: tcpAddr.Port})
		}(p)
	}

	wg.Wait()
	return sconf.Save()
}

func deployReverse(providers []sdk.Provider, origin string, ips []string) error {
	rconf, err := config.LoadReverseConfig()
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(len(providers))

	u, _ := url.Parse(origin)
	scheme := u.Scheme

	for _, p := range providers {
		go func(p sdk.Provider) {
			defer wg.Done()
			rp, ok := p.(sdk.ReverseProxyProvider)
			if !ok {
				logrus.Errorf("%s can't deploy reverse proxy", p.Name())
				return
			}

			opts := &sdk.ReverseProxyOpts{Origin: origin, Ips: ips}
			r, err := rp.DeployReverseProxy(opts)
			if err != nil {
				logrus.Error(err)
				return
			}

			whitelistIp := strings.Join(ips, ", ")
			if r.PluginId == "" {
				whitelistIp = "all"
			}

			api := fmt.Sprintf("%s://%s", scheme, r.ServiceDomain)
			record := &config.ReverseRecord{
				Provider:  r.Provider,
				Region:    r.Region,
				ApiId:     r.ApiId,
				Api:       api,
				Origin:    r.Origin,
				ServiceId: r.ServiceId,
				PluginId:  r.PluginId,
				Ips:       ips,
			}
			rconf.Add(record)
			logrus.Infof("[success] %s.%s: %s - %s : accessible from %v", rp.Name(), rp.Region(), r.Origin, api, whitelistIp)
		}(p)
	}

	wg.Wait()
	return rconf.Save()
}

func randomString(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	s := fmt.Sprintf("%X", b)
	return s
}