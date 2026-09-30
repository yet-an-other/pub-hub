package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type cli struct {
	in        io.Reader
	out, err  io.Writer
	client    *http.Client
	sleep     func(time.Duration)
	configDir string
}
type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type failure struct {
	code, message string
	exit          int
}

func (e failure) Error() string        { return fmt.Sprintf("error: %s: %s", e.code, e.message) }
func local(code, message string) error { return failure{code, message, 2} }
func main() {
	c := cli{in: os.Stdin, out: os.Stdout, err: os.Stderr, client: &http.Client{Timeout: 10 * time.Minute}, sleep: time.Sleep}
	os.Exit(c.run(os.Args[1:]))
}
func (c *cli) run(args []string) int {
	var err error
	if len(args) == 0 {
		err = local("usage", "expected publish, list, delete, whoami or login")
	} else {
		switch args[0] {
		case "publish":
			err = c.publish(args[1:])
		case "list":
			err = c.list(args[1:])
		case "delete":
			err = c.delete(args[1:])
		case "whoami":
			if len(args) != 1 {
				err = local("usage", "whoami takes no arguments")
			} else {
				err = c.whoami()
			}
		case "login":
			if len(args) != 1 {
				err = local("usage", "login takes no arguments")
			} else {
				err = c.login()
			}
		default:
			err = local("usage", "unknown command "+args[0])
		}
	}
	if err == nil {
		return 0
	}
	var f failure
	if errors.As(err, &f) {
		fmt.Fprintln(c.err, f.Error())
		return f.exit
	}
	fmt.Fprintf(c.err, "error: request_failed: %v\n", err)
	return 1
}
func (c *cli) transport() *http.Client {
	if c.client != nil {
		return c.client
	}
	return &http.Client{Timeout: 10 * time.Minute}
}
func (c *cli) pause(d time.Duration) {
	if c.sleep != nil {
		c.sleep(d)
	} else {
		time.Sleep(d)
	}
}
func (c *cli) call(method, path, token, base, contentType string, body func() (io.ReadCloser, error), createOnly bool) ([]byte, error) {
	for attempt := 0; attempt < 4; attempt++ {
		var reader io.ReadCloser
		var err error
		if body != nil {
			reader, err = body()
			if err != nil {
				return nil, err
			}
		}
		req, err := http.NewRequest(method, strings.TrimRight(base, "/")+path, reader)
		if err != nil {
			if reader != nil {
				reader.Close()
			}
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if createOnly {
			req.Header.Set("If-None-Match", "*")
		}
		resp, err := c.transport().Do(req)
		if err != nil {
			if reader != nil {
				reader.Close()
			}
			return nil, err
		}
		var response io.Reader = io.LimitReader(resp.Body, 2<<20)
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			response = resp.Body
		}
		data, readErr := io.ReadAll(response)
		resp.Body.Close()
		if reader != nil {
			reader.Close()
		}
		if readErr != nil {
			return nil, readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return data, nil
		}
		var problem apiError
		_ = json.Unmarshal(data, &problem)
		code := problem.Error.Code
		message := problem.Error.Message
		if code == "" {
			code = "http_error"
			message = resp.Status
		}
		if (code == "busy" || resp.StatusCode == 503) && attempt < 3 {
			delay := 5 * time.Second
			if value := resp.Header.Get("Retry-After"); value != "" {
				if secs, e := time.ParseDuration(value + "s"); e == nil && secs >= 0 {
					delay = secs
				} else if date, e := http.ParseTime(value); e == nil {
					delay = time.Until(date)
					if delay < 0 {
						delay = 0
					}
				}
			}
			c.pause(delay)
			continue
		}
		exit := 1
		switch {
		case code == "shape_conflict" || code == "nesting_conflict" || code == "exists":
			exit = 3
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			exit = 4
		case code == "busy" || resp.StatusCode == 503:
			exit = 5
		}
		return nil, failure{code, message, exit}
	}
	return nil, errors.New("retry exhausted")
}
func (c *cli) whoami() error {
	base, token, err := c.credentials()
	if err != nil {
		return err
	}
	data, err := c.call("GET", "/api/whoami", token, base, "", nil, false)
	if err != nil {
		return err
	}
	var result map[string]any
	if err = json.Unmarshal(data, &result); err != nil {
		return err
	}
	label, _ := result["publisher"].(string)
	if label == "" {
		label, _ = result["label"].(string)
	}
	fmt.Fprintln(c.out, label)
	return nil
}
func jsonOutput(out io.Writer, data []byte) error {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, compact.String())
	return err
}
