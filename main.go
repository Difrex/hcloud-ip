package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/syslog"
	"os"
	"time"

	"github.com/hetznercloud/hcloud-go/hcloud"
)

const (
	levelNotice uint = iota
	levelWarn
	levelError
	levelCrit

	syslogTag string = "hcloud-flip"
)

var sloggers map[uint]*syslog.Writer

func slogNotice(t string, args ...any) {
	if l, ok := sloggers[levelNotice]; ok {
		l.Notice(fmt.Sprintf(t, args...))
	}
}

func slogWarn(t string, args ...any) {
	if l, ok := sloggers[levelWarn]; ok {
		l.Warning(fmt.Sprintf(t, args...))
	}
}

func slogError(t string, args ...any) {
	if l, ok := sloggers[levelError]; ok {
		l.Err(fmt.Sprintf(t, args...))
	}
}

func slogCrit(t string, args ...any) {
	if l, ok := sloggers[levelCrit]; ok {
		l.Crit(fmt.Sprintf(t, args...))
	}
}

func getServer(ctx context.Context, client *hcloud.Client, name string) (*hcloud.Server, error) {
	c, cancel := context.WithCancel(ctx)
	defer cancel()

	server, _, err := client.Server.GetByName(c, name)
	if err != nil {
		return nil, err
	}

	if server == nil {
		slogCrit("server with name %v was not found!", name)
		return nil, fmt.Errorf("Server %s not found", name)
	}

	slogNotice("Server called %v was found\n", server.Name)
	response := fmt.Sprintf("Server called %v was found\n", server.Name)
	log.Println(response)

	return server, nil
}

func getIP(ctx context.Context, client *hcloud.Client, name string) (*hcloud.FloatingIP, error) {
	c, cancel := context.WithCancel(ctx)
	defer cancel()

	ip, _, err := client.FloatingIP.Get(c, name)
	if err != nil {
		slogCrit("error retrieving floating ip: %s\n", err)
		return nil, err
	}

	if ip == nil {
		slogCrit("Floating IP %s not found", name)
		return nil, fmt.Errorf("Floating IP %s not found", name)
	}

	slogNotice("Floating IP %s found", name)

	return ip, nil
}

const (
	lockFile string = "/tmp/.hcloud-ip.lock"
)

func lock() {
	f, err := os.Create(lockFile)
	if errors.Is(err, os.ErrExist) {
		slogError("Can't create lock file, file exist")
	}
	defer f.Close()
	slogNotice("Got session lock")
}

func unlock() {
	if err := os.Remove(lockFile); err != nil {
		slogError("Can't remove lock: %s", err)
	}
	slogNotice("Session unlocked")
}

func locked() bool {
	if _, err := os.Stat(lockFile); errors.Is(err, os.ErrNotExist) {
		return false
	}

	return true
}

func main() {
	if locked() {
		slogNotice("Session locked, exit")
		return
	}
	lock()

	apiKey := flag.String("key", "", "API Key")
	floatIP := flag.String("ip", "", "Name of Floating IP")
	hostname := flag.String("hostname", "", "Name of the cloud server")
	monitor := flag.Bool("monitor", false, "Used to check is floating ip assigned to the server")
	flag.Parse()

	if *apiKey == "" {
		unlock()
		slogCrit("No API Key specified!")
		log.Fatalf("No API Key specified!")
	}

	if *floatIP == "" {
		unlock()
		slogCrit("No Floating IP specified!")
		log.Fatalf("No Floating IP specified!")
	}

	name := *hostname
	if name == "" {
		n, err := os.Hostname()
		if err != nil {
			unlock()
			slogCrit("Error: %s", err.Error())
			panic(err)
		}
		name = n
	}

	ctx := context.Background()

	client := hcloud.NewClient(hcloud.WithToken(*apiKey))

	if *monitor {
		if !isFloatingIPAssigned(ctx, client, name) {
			unlock()
			os.Exit(2)
		}
		unlock()
		os.Exit(0)
	}

	if err := assignIP(ctx, client, *floatIP, name); err != nil {
		slogCrit("Error: Can't assign Floating IP %s to server %s", *floatIP, name)
		log.Fatalf("Error: Can't assign Floating IP %s to server %s", *floatIP, name)
		unlock()
	}
	unlock()
}

func assignIP(ctx context.Context, client *hcloud.Client, ipName, name string) error {
	slogNotice("Trying to assign Floating IP %s to %s", ipName, name)

	c, cancel := context.WithCancel(ctx)
	defer cancel()

	ip, err := getIP(c, client, ipName)
	if err != nil {
		return err
	}

	server, err := getServer(c, client, name)
	if err != nil {
		return err
	}

	var counter int
	var ok bool
	maxRetries := 10
	timeout := time.Duration(time.Second * 5)
	for !ok {
		_, res, err := client.FloatingIP.Assign(c, ip, server)
		defer res.Body.Close()

		if err != nil {
			slogError("Error assigning Floating ip: %s", err.Error())
			b, err := io.ReadAll(res.Body)
			if err != nil {
				slogError("Error can't read error response from API: %s", err.Error())
			} else {
				slogError("Error assigning Floating ip: %s", string(b))
			}
			if counter <= maxRetries {
				counter++
				slogWarn("Retry assigning floating IP %s to %s after %f seconds", ipName, name, timeout.Seconds())
				time.Sleep(timeout)
				timeout = timeout + time.Duration(time.Second*5)
				continue
			}
		}

		if isFloatingIPAssigned(c, client, name) {
			ok = true
			break
		}

		if counter == maxRetries {
			slogError("Error can't assign Floating IP, max retries count(%d) is reached", maxRetries)
			return fmt.Errorf("Can't assign Floating IP after %d retries", maxRetries)
		}

		slogWarn("Floating IP still not assigned to %s, retry after %f seconds", name, timeout.Seconds())
		time.Sleep(timeout)
		timeout = timeout + time.Duration(time.Second*5)
		counter++
	}

	slogNotice("Floating IP %s succesefully assigned to %s", ipName, name)
	return nil
}

func isFloatingIPAssigned(ctx context.Context, client *hcloud.Client, name string) bool {
	c, cancel := context.WithCancel(ctx)
	defer cancel()

	s, err := getServer(c, client, name)
	if err != nil {
		slogError("Can't get server: %s", err)
		return false
	}

	if len(s.PublicNet.FloatingIPs) > 0 {
		slogNotice("Floating IPs found on server %s", name)
		return true
	}

	return false
}

func init() {
	sloggers = make(map[uint]*syslog.Writer)

	noticeLogger, err := syslog.New(syslog.LOG_NOTICE, syslogTag)
	if err != nil {
		log.Printf("Error creating syslog NOTICE logger: %s\n", err)
		return
	}

	warnLogger, err := syslog.New(syslog.LOG_WARNING, syslogTag)
	if err != nil {
		log.Printf("Error creating syslog WARN logger: %s\n", err)
	}

	errLogger, err := syslog.New(syslog.LOG_ERR, syslogTag)
	if err != nil {
		log.Printf("Error creating syslog ERR logger: %s\n", err)
	}

	critLogger, err := syslog.New(syslog.LOG_CRIT, syslogTag)
	if err != nil {
		log.Printf("Error creating syslog CRIT logger: %s\n", err)
	}

	sloggers[levelNotice] = noticeLogger
	sloggers[levelWarn] = warnLogger
	sloggers[levelError] = errLogger
	sloggers[levelCrit] = critLogger
}
