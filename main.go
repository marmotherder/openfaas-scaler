package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jessevdk/go-flags"

	providerTypes "github.com/openfaas/faas-provider/types"
	"github.com/openfaas/faas/gateway/metrics"
)

var opts struct {
	Verbose              []bool   `short:"v" long:"verbose" description:"Show verbose debug information"`
	DryRun               bool     `short:"d" long:"dry_run" description:"Should scaling be run in dry run mode, scaling events will be shown, but not committed"`
	GatewayURI           string   `short:"g" long:"gateway_uri" description:"Full URI to the openfaas gateway" default:"http://gateway:8080"`
	GatewayHeaders       []string `short:"a" long:"gateway_header" description:"Additional headers to use when calling the gateway, eg. authentication"`
	PrometheusHost       string   `short:"p" long:"prometheus_host" description:"Full URI to the openfaas gateway" default:"prometheus"`
	PrometheusPort       int      `short:"o" long:"prometheus_port" description:"Full URI to the openfaas gateway" default:"9090"`
	PollingFrequency     int      `short:"f" long:"polling_frequency" description:"Polling frequency against scaling in seconds" default:"30"`
	DefaultScaleInterval int      `short:"i" long:"default_scale_interval" description:"Default interval period between scaling events in seconds" default:"320"`
	IgnoreLabels         bool     `short:"n" long:"ignore_labels" description:"Ignore scaling labels and run for every function found"`
}

var appLogger logger

var client = http.DefaultClient

func main() {
	parseArgs()
	appLogger := defaultLogger{
		level: len(opts.Verbose),
	}

	appLogger.debug(opts)
	appLogger.info("Running first polling")
	pollFunctions()

	appLogger.info(fmt.Sprintf("Starting polling loop, running every %d seconds", opts.PollingFrequency))

	ticker := time.NewTicker(time.Duration(opts.PollingFrequency) * time.Second)
	for range ticker.C {
		ticker.Stop()
		pollFunctions()
		ticker.Reset(time.Duration(opts.PollingFrequency) * time.Second)
	}
}

func parseArgs() {
	_, err := flags.ParseArgs(&opts, os.Args)
	if err != nil {
		usedHelp := func() bool {
			for _, arg := range os.Args {
				if arg == "-h" || arg == "--help" || arg == "help" {
					return true
				}
			}
			return false
		}
		if usedHelp() {
			os.Exit(0)
		}
		log.Fatalln(err.Error())
	}
}

func pollFunctions() {
	appLogger.info("Polling functions")

	functions := []providerTypes.FunctionStatus{}
	if err := callGateway(http.MethodGet, "system/functions", &functions, nil, 200); err != nil {
		log.Println(err.Error())
	}

	idleFunctions := listIdleFunctions(functions)
	if len(idleFunctions) == 0 {
		appLogger.info("No idle functions found, stopping!")
		return
	}

	for _, idleFunction := range idleFunctions {
		if idleFunction.Replicas > 0 {
			err := scaleFunction(idleFunction.Name, 0)
			if err != nil {
				log.Println(err.Error())
			}
		}
	}
	appLogger.info("Finished polling functions")
}

func scaleFunction(fnName string, replicas uint64) error {
	req := providerTypes.ScaleServiceRequest{
		ServiceName: fnName,
		Replicas:    replicas,
	}

	if opts.DryRun {
		appLogger.info("*DRY RUN*")
		appLogger.info(fmt.Sprintf("Would be scaling %s replicas to %d", fnName, replicas))
		return nil
	}

	appLogger.info(fmt.Sprintf("Scaling %s replicas to %d", fnName, replicas))
	if err := callGateway(http.MethodPost, fmt.Sprintf("system/scale-function/%s", fnName), nil, req, 202); err != nil {
		return err
	}

	return nil
}

func listIdleFunctions(functions []providerTypes.FunctionStatus) (idleFunctions []providerTypes.FunctionStatus) {
	query := metrics.NewPrometheusQuery(opts.PrometheusHost, opts.PrometheusPort, client)
	appLogger.debug(fmt.Sprintf("creating prometheus client with host: %s and port: %d", opts.PrometheusHost, opts.PrometheusPort))
	duration := fmt.Sprintf("%ds", opts.DefaultScaleInterval)
	appLogger.debug(fmt.Sprintf("idle duration set to %s", duration))

	c := make(chan providerTypes.FunctionStatus)
	wg := sync.WaitGroup{}
	for _, function := range functions {
		wg.Add(1)
		go func(function providerTypes.FunctionStatus) {
			defer wg.Done()
			fnName := fmt.Sprintf("%s.%s", function.Name, function.Namespace)

			if !canZero(fnName, *function.Labels) {
				return
			}

			queryReq := url.QueryEscape(fmt.Sprintf(`sum(rate(gateway_function_invocation_total{function_name="%s", code=~".*"}[%s])) by (code, function_name)`, fnName, duration))
			appLogger.trace("calling prometheus with query:")
			appLogger.trace(queryReq)

			resp, err := query.Fetch(queryReq)
			if err != nil {
				appLogger.debug("failed to query prometheus")
				appLogger.debug(err.Error())
			}

			appLogger.trace("prometheus query response:")
			appLogger.trace(resp)

			if len(resp.Data.Result) <= 0 {
				c <- function
				return
			}

			if !hasActiveResult(resp) {
				c <- function
			}
		}(function)
	}

	go func() {
		defer close(c)
		wg.Wait()
	}()

	for function := range c {
		idleFunctions = append(idleFunctions, function)
	}

	return
}

func callGateway(method, path string, result interface{}, data interface{}, statusCodes ...int) error {
	appLogger.trace("http client settings:")
	appLogger.trace(client.Transport)

	appLogger.debug("using data:")
	appLogger.debug(data)
	var body io.Reader
	body = nil
	if data != nil {
		dataBody, err := json.Marshal(data)
		if err != nil {
			return err
		}
		appLogger.trace("encoded data payload:")
		appLogger.trace(dataBody)
		body = bytes.NewBuffer(dataBody)
	}

	req, err := http.NewRequest(method, fmt.Sprintf("%s/%s", opts.GatewayURI, path), body)
	if err != nil {
		return err
	}

	appLogger.trace("request object:")
	appLogger.trace(req)

	for _, header := range opts.GatewayHeaders {
		headerKV := strings.SplitN(header, ":", 2)
		if len(headerKV) != 2 {
			return fmt.Errorf("invalid header '%s' provided", header)
		}

		headerKey := strings.TrimSpace(headerKV[0])
		headerValue := strings.TrimSpace(headerKV[1])

		appLogger.debug("adding header to request:")
		appLogger.debug(header)
		req.Header.Add(headerKey, headerValue)
	}

	basicAuthUser := os.Getenv("BASIC_AUTH_USER")
	basicAuthPassword := os.Getenv("BASIC_AUTH_PASSWORD")

	if basicAuthUser != "" && basicAuthPassword != "" {
		appLogger.debug("setting basic auth header via environment variables")
		req.SetBasicAuth(basicAuthUser, basicAuthPassword)
	}

	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}
	appLogger.trace("api response:")
	appLogger.trace(resp)

	if !validStatus(resp.StatusCode, statusCodes...) {
		bodyBytes, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			appLogger.debug("failed to read body response from gateway")
			return err
		}

		appLogger.debug("bad gateway response, body returned:")
		appLogger.debug(string(bodyBytes))
		return fmt.Errorf("invalid response from gateway, with status %d", resp.StatusCode)
	}

	if resp.Body != nil && result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return err
		}

		appLogger.debug("parsed gateway response:")
		appLogger.debug(result)
	}

	return nil
}

func canZero(fnName string, labels map[string]string) bool {
	if opts.IgnoreLabels {
		return true
	}
	for key, value := range labels {
		if key == "com.openfaas.scale.zero" && value == "true" {
			appLogger.debug(fmt.Sprintf("found scale to zero header for function %s", fnName))
			return true
		}
	}
	appLogger.debug(fmt.Sprintf("no scale to zero header found for function %s", fnName))
	return false
}

func hasActiveResult(resp *metrics.VectorQueryResponse) bool {
	for _, result := range resp.Data.Result {
		if len(result.Value) < 2 {
			continue
		}
		resultValue, ok := result.Value[1].(string)
		if !ok {
			continue
		}

		if resultValue != "0" && resultValue != "0.0" {
			return true
		}
	}
	return false
}

func validStatus(statusCode int, validStatusCodes ...int) bool {
	for _, validStatusCode := range validStatusCodes {
		if statusCode == validStatusCode {
			return true
		}
	}
	return false
}

type logger interface {
	getLevel() int
	info(message interface{})
	debug(message interface{})
	trace(message interface{})
}

type defaultLogger struct {
	level int
}

func (l defaultLogger) getLevel() int {
	return l.level
}

func (l defaultLogger) info(message interface{}) {
	log.Println(message)
}

func (l defaultLogger) debug(message interface{}) {
	if l.level > 0 {
		log.Println(message)
	}
}

func (l defaultLogger) trace(message interface{}) {
	if l.level > 1 {
		log.Println(message)
	}
}
