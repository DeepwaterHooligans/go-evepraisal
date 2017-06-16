package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/evepraisal/go-evepraisal"
	"github.com/evepraisal/go-evepraisal/bolt"
	"github.com/evepraisal/go-evepraisal/esi"
	"github.com/evepraisal/go-evepraisal/management"
	"github.com/evepraisal/go-evepraisal/parsers"
	"github.com/evepraisal/go-evepraisal/staticdump"
	"github.com/evepraisal/go-evepraisal/typedb"
	"github.com/evepraisal/go-evepraisal/web"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/newrelic/go-agent"
	"github.com/spf13/viper"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/oauth2"
)

func appMain() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	log.Println("Starting price DB")
	priceDB, err := bolt.NewPriceDB(filepath.Join(viper.GetString("db_path"), "prices"))
	if err != nil {
		log.Fatalf("Couldn't start price database: %s", err)
	}
	defer func() {
		err := priceDB.Close()
		if err != nil {
			log.Fatalf("Problem closing priceDB: %s", err)
		}
	}()

	httpCache, err := bolt.NewHTTPCache(filepath.Join(viper.GetString("db_path"), "httpcache"))
	if err != nil {
		log.Fatalf("Couldn't start httpCache: %s", err)
	}
	defer func() {
		err := httpCache.Close()
		if err != nil {
			log.Fatalf("Problem closing httpCache: %s", err)
		}
	}()

	defer func() {
		err := priceDB.Close()
		if err != nil {
			log.Fatalf("Problem closing priceDB: %s", err)
		}
	}()

	priceFetcher, err := esi.NewPriceFetcher(priceDB, viper.GetString("esi_baseurl"), httpCache)
	if err != nil {
		log.Fatalf("Couldn't start price fetcher: %s", err)
	}
	defer func() {
		err := priceFetcher.Close()
		if err != nil {
			log.Fatalf("Problem closing priceDB: %s", err)
		}
	}()

	log.Println("Starting appraisal DB")
	appraisalDB, err := bolt.NewAppraisalDB(filepath.Join(viper.GetString("db_path"), "appraisals"))
	if err != nil {
		log.Fatalf("Couldn't start appraisal database: %s", err)
	}
	defer func() {
		err := appraisalDB.Close()
		if err != nil {
			log.Fatalf("Problem closing appraisalDB: %s", err)
		}
	}()

	app := &evepraisal.App{
		AppraisalDB: appraisalDB,
		PriceDB:     priceDB,
	}

	if viper.GetString("newrelic_license-key") != "" {
		newRelicConfig := newrelic.NewConfig(viper.GetString("newrelic_app-name"), viper.GetString("newrelic_license-key"))
		newRelicApplication, err := newrelic.NewApplication(newRelicConfig)
		if err != nil {
			log.Fatalf("Problem configuring new relic: %s", err)
		}

		app.NewRelicApplication = newRelicApplication
	}

	log.Println("Starting type fetcher")
	staticFetcher, err := staticdump.NewStaticFetcher(viper.GetString("db_path"), func(typeDB typedb.TypeDB) {
		oldTypeDB := app.TypeDB
		app.TypeDB = typeDB
		app.Parser = evepraisal.NewContextMultiParser(
			typeDB,
			[]parsers.Parser{
				parsers.ParseKillmail,
				parsers.ParseEFT,
				parsers.ParseFitting,
				parsers.ParseLootHistory,
				parsers.ParsePI,
				parsers.ParseViewContents,
				parsers.ParseWallet,
				parsers.ParseSurveyScan,
				parsers.ParseContract,
				parsers.ParseAssets,
				parsers.ParseIndustry,
				parsers.ParseCargoScan,
				parsers.ParseDScan,
				parsers.NewContextListingParser(typeDB),
				parsers.NewHeuristicParser(typeDB),
			})

		if oldTypeDB != nil {
			log.Println("closeing old typedb")
			oldTypeDB.Close()
			log.Println("closed old typedb")
		}
	})
	if err != nil {
		log.Fatalf("Couldn't start static fetcher: %s", err)
	}
	defer func() {
		err := staticFetcher.Close()
		if err != nil {
			log.Fatalf("Problem closing static fetcher: %s", err)
		}

		if app.TypeDB != nil {
			err = app.TypeDB.Close()
			if err != nil {
				log.Fatalf("Problem closing typeDB: %s", err)
			}
		}
	}()

	webContext := web.NewContext(app)
	webContext.BaseURL = strings.TrimSuffix(viper.GetString("base-url"), "/")
	webContext.ExtraJS = viper.GetString("extra-js")
	webContext.AdBlock = viper.GetString("ad-block")
	if viper.GetString("cookie-auth-key") != "" {
		webContext.CookieStore = sessions.NewCookieStore(
			[]byte(viper.GetString("cookie-auth-key")),
			[]byte(viper.GetString("cookie-encryption-key")))
	} else {
		webContext.CookieStore = sessions.NewCookieStore(securecookie.GenerateRandomKey(32))
	}
	if viper.GetString("sso-client-id") != "" {
		webContext.OauthConfig = &oauth2.Config{
			ClientID:     viper.GetString("sso-client-id"),
			ClientSecret: viper.GetString("sso-client-secret"),
			Scopes:       []string{},
			Endpoint: oauth2.Endpoint{
				AuthURL:  viper.GetString("sso-authorize-url"),
				TokenURL: viper.GetString("sso-token-url"),
			},
			RedirectURL: viper.GetString("base-url") + "/oauthcallback",
		}
		webContext.OauthVerifyURL = viper.GetString("sso-verify-url")
	}

	app.WebContext = webContext

	servers := mustStartServers(app.WebContext.HTTPHandler())
	if err != nil {
		log.Fatalf("Problem starting https server: %s", err)
	}

	for _, server := range servers {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer server.Shutdown(stopCtx)
		go func() {
			time.Sleep(10 * time.Second)
			cancel()
		}()
	}

	startEnvironmentWatchers(app)

	log.Printf("Starting Management HTTP server (%s)", viper.GetString("management_addr"))
	mgmtServer := &http.Server{
		Addr:    viper.GetString("management_addr"),
		Handler: management.HTTPHandler(app),
	}
	defer mgmtServer.Close()
	go func() {
		err := mgmtServer.ListenAndServe()
		if err == http.ErrServerClosed {
			log.Println("Management HTTP server stopped")
		} else if err != nil {
			log.Fatalf("Management HTTP server failure: %s", err)
		}
	}()

	<-stop
	log.Println("Shutting down")
}

func mustStartServers(handler http.Handler) []*http.Server {
	servers := make([]*http.Server, 0)

	if viper.GetString("https_addr") != "" {
		log.Printf("Starting HTTPS server (%s) (%s)", viper.GetString("https_addr"), viper.GetStringSlice("https_domain-whitelist"))

		autocertManager := autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(viper.GetStringSlice("https_domain-whitelist")...),
			Cache:      autocert.DirCache(filepath.Join(viper.GetString("db_path"), "certs")),
		}

		server := &http.Server{
			Addr:      viper.GetString("https_addr"),
			Handler:   handler,
			TLSConfig: &tls.Config{GetCertificate: autocertManager.GetCertificate},
		}
		servers = append(servers, server)

		go func() {
			err := server.ListenAndServeTLS("", "")
			if err == http.ErrServerClosed {
				log.Println("HTTPS server stopped")
			} else if err != nil {
				log.Fatalf("HTTPS server failure: %s", err)
			}
		}()
		time.Sleep(1 * time.Second)
	}

	if viper.GetString("http_addr") != "" {
		log.Printf("Starting HTTP server (%s)", viper.GetString("http_addr"))

		server := &http.Server{
			Addr:    viper.GetString("http_addr"),
			Handler: handler,
		}
		servers = append(servers, server)

		go func() {
			err := server.ListenAndServe()
			if err == http.ErrServerClosed {
				log.Println("HTTP server stopped")
			} else if err != nil {
				log.Fatalf("HTTP server failure: %s", err)
			}
		}()
	}

	return servers
}
