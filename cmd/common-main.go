/*
 * MinIO Cloud Storage, (C) 2017-2019 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	dns2 "github.com/miekg/dns"
	"github.com/minio/cli"
	"github.com/minio/minio-go/v7/pkg/set"
	"github.com/minio/minio/cmd/config"
	"github.com/minio/minio/cmd/crypto"
	xhttp "github.com/minio/minio/cmd/http"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/auth"
	"github.com/minio/minio/pkg/certs"
	"github.com/minio/minio/pkg/console"
	"github.com/minio/minio/pkg/env"
	"github.com/minio/minio/pkg/handlers"
	"github.com/minio/minio/pkg/kms"
)

// serverDebugLog will enable debug printing
var serverDebugLog = env.Get("_MINIO_SERVER_DEBUG", config.EnableOff) == config.EnableOn

func init() {
	rand.Seed(time.Now().UTC().UnixNano())

	logger.Init(GOPATH, GOROOT)
	logger.RegisterError(config.FmtError)

	// Inject into config package.
	config.Logger.Info = logger.Info
	config.Logger.LogIf = logger.LogIf

	if IsKubernetes() || IsDocker() || IsBOSH() || IsDCOS() || IsKubernetesReplicaSet() || IsPCFTile() {
		// 30 seconds matches the orchestrator DNS TTLs, have
		// a 5 second timeout to lookup from DNS servers.
		globalDNSCache = xhttp.NewDNSCache(30*time.Second, 5*time.Second, logger.LogOnceIf)
	} else {
		// On bare-metals DNS do not change often, so it is
		// safe to assume a higher timeout upto 10 minutes.
		globalDNSCache = xhttp.NewDNSCache(10*time.Minute, 5*time.Second, logger.LogOnceIf)
	}

	initGlobalContext()

	globalForwarder = handlers.NewForwarder(&handlers.Forwarder{
		PassHost:     true,
		RoundTripper: newGatewayHTTPTransport(1 * time.Hour),
		Logger: func(err error) {
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.LogIf(GlobalContext, err)
			}
		},
	})

	globalTransitionState = newTransitionState()

	console.SetColor("Debug", color.New())

	gob.Register(StorageErr(""))
}

func verifyObjectLayerFeatures(name string, objAPI ObjectLayer) {
	if (GlobalKMS != nil) && !objAPI.IsEncryptionSupported() {
		logger.Fatal(errInvalidArgument,
			"Encryption support is requested but '%s' does not support encryption", name)
	}

	if strings.HasPrefix(name, "gateway") {
		if GlobalGatewaySSE.IsSet() && GlobalKMS == nil {
			uiErr := config.ErrInvalidGWSSEEnvValue(nil).Msg("MINIO_GATEWAY_SSE set but KMS is not configured")
			logger.Fatal(uiErr, "Unable to start gateway with SSE")
		}
	}

	globalCompressConfigMu.Lock()
	if globalCompressConfig.Enabled && !objAPI.IsCompressionSupported() {
		logger.Fatal(errInvalidArgument,
			"Compression support is requested but '%s' does not support compression", name)
	}
	globalCompressConfigMu.Unlock()
}

// Check for updates and print a notification message
func checkUpdate(mode string) {
	updateURL := minioReleaseInfoURL
	if runtime.GOOS == globalWindowsOSName {
		updateURL = minioReleaseWindowsInfoURL
	}

	u, err := url.Parse(updateURL)
	if err != nil {
		return
	}

	// Its OK to ignore any errors during doUpdate() here.
	crTime, err := GetCurrentReleaseTime()
	if err != nil {
		return
	}

	_, lrTime, err := getLatestReleaseTime(u, 2*time.Second, mode)
	if err != nil {
		return
	}

	var older time.Duration
	var downloadURL string
	if lrTime.After(crTime) {
		older = lrTime.Sub(crTime)
		downloadURL = getDownloadURL(releaseTimeToReleaseTag(lrTime))
	}

	updateMsg := prepareUpdateMessage(downloadURL, older)
	if updateMsg == "" {
		return
	}

	logStartupMessage(prepareUpdateMessage("Run `mc admin update`", lrTime.Sub(crTime)))
}

func newConfigDirFromCtx(ctx *cli.Context, option string, getDefaultDir func() string) (*ConfigDir, bool) {
	var dir string
	var dirSet bool

	switch {
	case ctx.IsSet(option):
		dir = ctx.String(option)
		dirSet = true
	case ctx.GlobalIsSet(option):
		dir = ctx.GlobalString(option)
		dirSet = true
		// cli package does not expose parent's option option.  Below code is workaround.
		if dir == "" || dir == getDefaultDir() {
			dirSet = false // Unset to false since GlobalIsSet() true is a false positive.
			if ctx.Parent().GlobalIsSet(option) {
				dir = ctx.Parent().GlobalString(option)
				dirSet = true
			}
		}
	default:
		// Neither local nor global option is provided.  In this case, try to use
		// default directory.
		dir = getDefaultDir()
		if dir == "" {
			logger.FatalIf(errInvalidArgument, "%s option must be provided", option)
		}
	}

	if dir == "" {
		logger.FatalIf(errors.New("empty directory"), "%s directory cannot be empty", option)
	}

	// Disallow relative paths, figure out absolute paths.
	dirAbs, err := filepath.Abs(dir)
	logger.FatalIf(err, "Unable to fetch absolute path for %s=%s", option, dir)

	logger.FatalIf(mkdirAllIgnorePerm(dirAbs), "Unable to create directory specified %s=%s", option, dir)

	return &ConfigDir{path: dirAbs}, dirSet
}

func handleCommonCmdArgs(ctx *cli.Context) {

	// Get "json" flag from command line argument and
	// enable json and quite modes if json flag is turned on.
	globalCLIContext.JSON = ctx.IsSet("json") || ctx.GlobalIsSet("json")
	if globalCLIContext.JSON {
		logger.EnableJSON()
	}

	// Get quiet flag from command line argument.
	globalCLIContext.Quiet = ctx.IsSet("quiet") || ctx.GlobalIsSet("quiet")
	if globalCLIContext.Quiet {
		logger.EnableQuiet()
	}

	// Get anonymous flag from command line argument.
	globalCLIContext.Anonymous = ctx.IsSet("anonymous") || ctx.GlobalIsSet("anonymous")
	if globalCLIContext.Anonymous {
		logger.EnableAnonymous()
	}

	// Fetch address option
	globalCLIContext.Addr = ctx.GlobalString("address")
	if globalCLIContext.Addr == "" || globalCLIContext.Addr == ":"+GlobalMinioDefaultPort {
		globalCLIContext.Addr = ctx.String("address")
	}

	// Check "no-compat" flag from command line argument.
	globalCLIContext.StrictS3Compat = true
	if ctx.IsSet("no-compat") || ctx.GlobalIsSet("no-compat") {
		globalCLIContext.StrictS3Compat = false
	}

	// Set all config, certs and CAs directories.
	var configSet, certsSet bool
	globalConfigDir, configSet = newConfigDirFromCtx(ctx, "config-dir", defaultConfigDir.Get)
	globalCertsDir, certsSet = newConfigDirFromCtx(ctx, "certs-dir", defaultCertsDir.Get)

	// Remove this code when we deprecate and remove config-dir.
	// This code is to make sure we inherit from the config-dir
	// option if certs-dir is not provided.
	if !certsSet && configSet {
		globalCertsDir = &ConfigDir{path: filepath.Join(globalConfigDir.Get(), certsDir)}
	}

	globalCertsCADir = &ConfigDir{path: filepath.Join(globalCertsDir.Get(), certsCADir)}

	logger.FatalIf(mkdirAllIgnorePerm(globalCertsCADir.Get()), "Unable to create certs CA directory at %s", globalCertsCADir.Get())
}

func handleCommonEnvVars() {
	wormEnabled, err := config.LookupWorm()
	if err != nil {
		logger.Fatal(config.ErrInvalidWormValue(err), "Invalid worm configuration")
	}
	if wormEnabled {
		logger.Fatal(errors.New("WORM is deprecated"), "global MINIO_WORM support is removed, please downgrade your server or migrate to https://github.com/minio/minio/tree/master/docs/retention")
	}

	globalBrowserEnabled, err = config.ParseBool(env.Get(config.EnvBrowser, config.EnableOn))
	if err != nil {
		logger.Fatal(config.ErrInvalidBrowserValue(err), "Invalid MINIO_BROWSER value in environment variable")
	}

	globalFSOSync, err = config.ParseBool(env.Get(config.EnvFSOSync, config.EnableOff))
	if err != nil {
		logger.Fatal(config.ErrInvalidFSOSyncValue(err), "Invalid MINIO_FS_OSYNC value in environment variable")
	}

	domains := env.Get(config.EnvDomain, "")
	if len(domains) != 0 {
		for _, domainName := range strings.Split(domains, config.ValueSeparator) {
			if _, ok := dns2.IsDomainName(domainName); !ok {
				logger.Fatal(config.ErrInvalidDomainValue(nil).Msg("Unknown value `%s`", domainName),
					"Invalid MINIO_DOMAIN value in environment variable")
			}
			globalDomainNames = append(globalDomainNames, domainName)
		}
		sort.Strings(globalDomainNames)
		lcpSuf := lcpSuffix(globalDomainNames)
		for _, domainName := range globalDomainNames {
			if domainName == lcpSuf && len(globalDomainNames) > 1 {
				logger.Fatal(config.ErrOverlappingDomainValue(nil).Msg("Overlapping domains `%s` not allowed", globalDomainNames),
					"Invalid MINIO_DOMAIN value in environment variable")
			}
		}
	}

	publicIPs := env.Get(config.EnvPublicIPs, "")
	if len(publicIPs) != 0 {
		minioEndpoints := strings.Split(publicIPs, config.ValueSeparator)
		var domainIPs = set.NewStringSet()
		for _, endpoint := range minioEndpoints {
			if net.ParseIP(endpoint) == nil {
				// Checking if the IP is a DNS entry.
				addrs, err := net.LookupHost(endpoint)
				if err != nil {
					logger.FatalIf(err, "Unable to initialize MinIO server with [%s] invalid entry found in MINIO_PUBLIC_IPS", endpoint)
				}
				for _, addr := range addrs {
					domainIPs.Add(addr)
				}
			}
			domainIPs.Add(endpoint)
		}
		updateDomainIPs(domainIPs)
	} else {
		// Add found interfaces IP address to global domain IPS,
		// loopback addresses will be naturally dropped.
		domainIPs := mustGetLocalIP4()
		for _, host := range globalEndpoints.Hostnames() {
			domainIPs.Add(host)
		}
		updateDomainIPs(domainIPs)
	}

	// In place update is true by default if the MINIO_UPDATE is not set
	// or is not set to 'off', if MINIO_UPDATE is set to 'off' then
	// in-place update is off.
	globalInplaceUpdateDisabled = strings.EqualFold(env.Get(config.EnvUpdate, config.EnableOn), config.EnableOff)

	if env.IsSet(config.EnvAccessKey) || env.IsSet(config.EnvSecretKey) {
		cred, err := auth.CreateCredentials(env.Get(config.EnvAccessKey, ""), env.Get(config.EnvSecretKey, ""))
		if err != nil {
			logger.Fatal(config.ErrInvalidCredentials(err),
				"Unable to validate credentials inherited from the shell environment")
		}
		globalActiveCred = cred
	}

	if env.IsSet(config.EnvRootUser) || env.IsSet(config.EnvRootPassword) {
		cred, err := auth.CreateCredentials(env.Get(config.EnvRootUser, ""), env.Get(config.EnvRootPassword, ""))
		if err != nil {
			logger.Fatal(config.ErrInvalidCredentials(err),
				"Unable to validate credentials inherited from the shell environment")
		}
		globalActiveCred = cred
	}

	if env.IsSet(config.EnvKMSSecretKey) && env.IsSet(config.EnvKESEndpoint) {
		logger.Fatal(errors.New("ambigious KMS configuration"), fmt.Sprintf("The environment contains %q as well as %q", config.EnvKMSSecretKey, config.EnvKESEndpoint))
	}
	switch {
	case env.IsSet(config.EnvKMSSecretKey) && env.IsSet(config.EnvKESEndpoint):
		logger.Fatal(errors.New("ambigious KMS configuration"), fmt.Sprintf("The environment contains %q as well as %q", config.EnvKMSSecretKey, config.EnvKESEndpoint))
	case env.IsSet(config.EnvKMSMasterKey) && env.IsSet(config.EnvKESEndpoint):
		logger.Fatal(errors.New("ambigious KMS configuration"), fmt.Sprintf("The environment contains %q as well as %q", config.EnvKMSMasterKey, config.EnvKESEndpoint))
	}
	if env.IsSet(config.EnvKMSSecretKey) {
		KMS, err := kms.Parse(env.Get(config.EnvKMSSecretKey, ""))
		if err != nil {
			logger.Fatal(err, "Unable to parse the KMS secret key inherited from the shell environment")
		}
		GlobalKMS = KMS
	} else if env.IsSet(config.EnvKMSMasterKey) {
		logger.LogIf(GlobalContext, errors.New("legacy KMS configuration"), fmt.Sprintf("The environment variable %q is deprecated and will be removed in the future", config.EnvKMSMasterKey))

		v := strings.SplitN(env.Get(config.EnvKMSMasterKey, ""), ":", 2)
		if len(v) != 2 {
			logger.Fatal(errors.New("invalid "+config.EnvKMSMasterKey), "Unable to parse the KMS secret key inherited from the shell environment")
		}
		secretKey, err := hex.DecodeString(v[1])
		if err != nil {
			logger.Fatal(err, "Unable to parse the KMS secret key inherited from the shell environment")
		}
		KMS, err := kms.New(v[0], secretKey)
		if err != nil {
			logger.Fatal(err, "Unable to parse the KMS secret key inherited from the shell environment")
		}
		GlobalKMS = KMS
	}
	if env.IsSet(config.EnvKESEndpoint) {
		kesEndpoints, err := crypto.ParseKESEndpoints(env.Get(config.EnvKESEndpoint, ""))
		if err != nil {
			logger.Fatal(err, "Unable to parse the KES endpoints inherited from the shell environment")
		}
		KMS, err := crypto.NewKes(crypto.KesConfig{
			Enabled:      true,
			Endpoint:     kesEndpoints,
			DefaultKeyID: env.Get(config.EnvKESKeyName, ""),
			CertFile:     env.Get(config.EnvKESClientCert, ""),
			KeyFile:      env.Get(config.EnvKESClientKey, ""),
			CAPath:       env.Get(config.EnvKESServerCA, globalCertsCADir.Get()),
			Transport:    newCustomHTTPTransportWithHTTP2(&tls.Config{RootCAs: globalRootCAs}, defaultDialTimeout)(),
		})
		if err != nil {
			logger.Fatal(err, "Unable to initialize a connection to KES as specified by the shell environment")
		}
		GlobalKMS = KMS
	}
}

func logStartupMessage(msg string) {
	if globalConsoleSys != nil {
		globalConsoleSys.Send(msg, string(logger.All))
	}
	logger.StartupMessage(msg)
}

func getTLSConfig() (x509Certs []*x509.Certificate, manager *certs.Manager, secureConn bool, err error) {
	if !(isFile(getPublicCertFile()) && isFile(getPrivateKeyFile())) {
		return nil, nil, false, nil
	}

	if x509Certs, err = config.ParsePublicCertFile(getPublicCertFile()); err != nil {
		return nil, nil, false, err
	}

	manager, err = certs.NewManager(GlobalContext, getPublicCertFile(), getPrivateKeyFile(), config.LoadX509KeyPair)
	if err != nil {
		return nil, nil, false, err
	}

	// MinIO has support for multiple certificates. It expects the following structure:
	//  certs/
	//   │
	//   ├─ public.crt
	//   ├─ private.key
	//   │
	//   ├─ example.com/
	//   │   │
	//   │   ├─ public.crt
	//   │   └─ private.key
	//   └─ foobar.org/
	//      │
	//      ├─ public.crt
	//      └─ private.key
	//   ...
	//
	// Therefore, we read all filenames in the cert directory and check
	// for each directory whether it contains a public.crt and private.key.
	// If so, we try to add it to certificate manager.
	root, err := os.Open(globalCertsDir.Get())
	if err != nil {
		return nil, nil, false, err
	}
	defer root.Close()

	files, err := root.Readdir(-1)
	if err != nil {
		return nil, nil, false, err
	}
	for _, file := range files {
		// Ignore all
		// - regular files
		// - "CAs" directory
		// - any directory which starts with ".."
		if file.Mode().IsRegular() || file.Name() == "CAs" || strings.HasPrefix(file.Name(), "..") {
			continue
		}
		if file.Mode()&os.ModeSymlink == os.ModeSymlink {
			file, err = os.Stat(filepath.Join(root.Name(), file.Name()))
			if err != nil {
				// not accessible ignore
				continue
			}
			if !file.IsDir() {
				continue
			}
		}

		var (
			certFile = filepath.Join(root.Name(), file.Name(), publicCertFile)
			keyFile  = filepath.Join(root.Name(), file.Name(), privateKeyFile)
		)
		if !isFile(certFile) || !isFile(keyFile) {
			continue
		}
		if err = manager.AddCertificate(certFile, keyFile); err != nil {
			err = fmt.Errorf("Unable to load TLS certificate '%s,%s': %w", certFile, keyFile, err)
			logger.LogIf(GlobalContext, err, logger.Minio)
		}
	}
	secureConn = true
	return x509Certs, manager, secureConn, nil
}

// contextCanceled returns whether a context is canceled.
func contextCanceled(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}
