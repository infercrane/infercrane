// Command fakeproofcontrol is a fake control plane for the proof gate's dry
// run. It serves one finished two-setup run per gate outcome, signed with a
// key it makes at start, and creates runs from optimize compare. It measures
// nothing and spends nothing. It writes its URL and public key as one JSON
// line to -ready-file once it listens.
package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/infercrane/infercrane/internal/passport"
	"github.com/infercrane/infercrane/internal/testtools/proofgatefixture"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "address to listen on")
	onCreate := flag.String("on-create", proofgatefixture.Pass, "scenario a run created by optimize compare ends in")
	apiKey := flag.String("api-key", "", "the only API key accepted; empty accepts any")
	readyFile := flag.String("ready-file", "", "write {\"url\":…,\"public_key\":…} here once listening")
	runningReads := flag.Int("running-reads", 0, "reads of an approved run that still report it measuring")
	flag.Parse()
	if _, err := proofgatefixture.Document(*onCreate, nil); err != nil {
		log.Fatal(err)
	}
	public, private, err := passport.GenerateKey()
	if err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatal(err)
	}
	ready, _ := json.Marshal(map[string]string{"url": "http://" + listener.Addr().String(), "public_key": base64.StdEncoding.EncodeToString(public), "key_id": passport.KeyID(public)})
	if *readyFile != "" {
		if err = os.WriteFile(*readyFile, ready, 0o600); err != nil {
			log.Fatal(err)
		}
	}
	fmt.Println(string(ready))
	server := &http.Server{Handler: &proofgatefixture.Server{Key: private, APIKey: *apiKey, OnCreate: *onCreate, RunningReads: *runningReads}, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.Serve(listener))
}
