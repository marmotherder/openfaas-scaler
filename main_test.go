package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"testing"

	providerTypes "github.com/openfaas/faas-provider/types"
	"github.com/stretchr/testify/assert"
)

func TestParseArgs(t *testing.T) {
	os.Args = []string{"debugger", "-vv"}

	parseArgs()

	appLogger := defaultLogger{
		level: len(opts.Verbose),
	}

	assert.Equal(t, appLogger.getLevel(), 2, "log level should be set to trace at 2")
}

func TestPollFunctions(t *testing.T) {
	client = newTestClient(func(req *http.Request) *http.Response {
		statusCode := http.StatusInternalServerError
		var respBody io.ReadCloser
		header := make(http.Header)

		if req.URL.Path == "/system/functions" {
			resp := []providerTypes.FunctionStatus{
				{
					Name:      "mock",
					Namespace: "mock",
					Labels:    &map[string]string{"com.openfaas.scale.zero": "true"},
					Replicas:  1,
				},
			}
			respData, err := json.Marshal(resp)
			if err != nil {
				t.Error(err.Error())
				t.FailNow()
			}
			statusCode = http.StatusOK
			respBody = ioutil.NopCloser(bytes.NewBuffer(respData))
		}
		if req.URL.Path == "/system/scale-function/mock" {
			statusCode = http.StatusAccepted
		}
		if req.URL.Path == "/api/v1/query" {
			resp := VectorQueryResponse{}
			result := VectorQueryResponseResult{}
			result.Metric.Code = "mock"
			result.Metric.FunctionName = "mock"
			result.Value = append(result.Value, "mock")
			result.Value = append(result.Value, "0")

			respData, err := json.Marshal(resp)
			if err != nil {
				t.Error(err.Error())
				t.FailNow()
			}

			statusCode = http.StatusOK
			respBody = ioutil.NopCloser(bytes.NewBuffer(respData))
		}
		return &http.Response{
			StatusCode: statusCode,
			Body:       respBody,
			Header:     header,
		}
	})

	appLogger = mockLogger{}

	pollFunctions()

	hasExpectedOutput := func() bool {
		for _, output := range mockLoggerOutput {
			if output == "Scaling mock replicas to 0" {
				return true
			}
		}
		return false
	}

	assert.True(t, hasExpectedOutput(), "output is missing expexted scaling event")
}

type VectorQueryResponse struct {
	Data struct {
		Result []VectorQueryResponseResult
	}
}

type VectorQueryResponseResult struct {
	Metric struct {
		Code         string `json:"code"`
		FunctionName string `json:"function_name"`
	}
	Value []interface{} `json:"value"`
}

type roundTripFunc func(req *http.Request) *http.Response

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req), nil
}

func newTestClient(fn roundTripFunc) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(fn),
	}
}

var mockLoggerOutput []string

type mockLogger struct{}

func (l mockLogger) getLevel() int {
	return 0
}

func (l mockLogger) info(message interface{}) {
	mockLoggerOutput = append(mockLoggerOutput, fmt.Sprint(message))
}

func (l mockLogger) debug(message interface{}) {
	mockLoggerOutput = append(mockLoggerOutput, fmt.Sprint(message))
}

func (l mockLogger) trace(message interface{}) {
	mockLoggerOutput = append(mockLoggerOutput, fmt.Sprint(message))
}
