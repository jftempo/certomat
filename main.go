package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/net/context"
)

var version = flag.Bool("version", false, "Show the version and exit.")
var domain = flag.String("domain", "", "The domain we are responsible for.")
var prod = flag.Bool("prod", false, "If this is set, then talk to the real production LetsEncrypt API.")
var cacheDir = flag.String("cache", "cache", "The location of the autocert cache directory.")

type Instance struct {
	Id, Name string
}

func getDomain(host string) (string, error) {
	x := strings.Split(host, ".")
	if len(x) > 1 {
		return strings.Join(x[1:], "."), nil
	} else {
		return "", fmt.Errorf("host %v has no dots", host)
	}
}

func HostWhitelistByDomains(doms map[string]bool) autocert.HostPolicy {
	return func(_ context.Context, host string) error {
		// FQDNs can end in dot
		// Except, see https://github.com/letsencrypt/boulder/issues/2367
		if strings.HasSuffix(host, ".") {
			host = host[0 : len(host)-1]
		}
		host = strings.ToLower(host)

		dom, err := getDomain(host)
		if err != nil {
			return err
		}

		if doms[dom] {
			return nil
		}
		return fmt.Errorf("certomat: domain %v not allowed", dom)
	}
}

func getNameFromCSR(csr []byte) string {
	cr, err := x509.ParseCertificateRequest(csr)
	if err != nil {
		return ""
	}
	return cr.Subject.CommonName
}

func httpError(w http.ResponseWriter, why string, code int) {
	log.Printf("err %v because: %v", code, why)
	http.Error(w, why, code)
}

var certbotMu sync.Mutex

// Handles URLs like https://certomatFqdn/get-cert-from-csr
//
// Read the CSR from the Body, send it to LetsEncrypt,
// send the cert back to them.
func getHandler(w http.ResponseWriter, r *http.Request) {
	log.Print(*r)

	if r.Method != "POST" {
		httpError(w, "post only", http.StatusMethodNotAllowed)
	}

	csr, err := ioutil.ReadAll(r.Body)
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	name := getNameFromCSR(csr)
	log.Print("certificate requested for ", name)

	tmpfile, err := ioutil.TempFile("", "csr")
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	defer os.Remove(tmpfile.Name()) // clean up

	if _, err := tmpfile.Write(csr); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmpfile.Close(); err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// turn csr into a cert by calling out to certbot
	args := []string{"certbot", "certonly", "--standalone",
		"--http-01-addr", certomatFqdn,
		"--csr", tmpfile.Name(), "--config-dir", "./config",
		"--work-dir", "./work", "--logs-dir", "./logs",
		"--non-interactive", "--preferred-challenges", "http",
		"-d", name}
	if !*prod {
		args = append(args, "--test-cert")
	}

	path, err := exec.LookPath("certbot")
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}

	cmd := &exec.Cmd{
		Path:   path,
		Args:   args,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}

	certbotMu.Lock()
	defer certbotMu.Unlock()

	err = cmd.Start()
	if err != nil {
		httpError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	err = cmd.Wait()
	if err != nil {
		httpError(w,
			fmt.Sprintf("certbot result code: %v", err.Error()),
			http.StatusInternalServerError)
		return
	}

	cert, err := ioutil.ReadFile("0000_cert.pem")
	if err != nil {
		httpError(w,
			fmt.Sprintf("cannot read cert: %v", err.Error()),
			http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain")
	w.Write(cert)

	cmd = exec.Command("sh", "-c", "rm 00*")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Run()
}

var mgr *autocert.Manager
var certomatFqdn string

var gitRevision = "(dev)"

func main() {
	flag.Parse()
	if *version {
		fmt.Println(gitRevision)
		return
	}

	if *domain == "" {
		fmt.Println("Domain argument is required. Exiting.")
		return
	}
	var domainNames = map[string]bool{
		*domain: true,
	}
	certomatFqdn := fmt.Sprintf("certomat.%v", *domain)

	// Default dirUrl to empty, to select the production API. Then
	// if the flag says we are not in prod mode, set the test mode URL.
	dirUrl := ""
	if !*prod {
		dirUrl = "https://acme-staging.api.letsencrypt.org/directory"
	}

	// Check that we can find certbot.
	_, err := exec.LookPath("certbot")
	if err != nil {
		log.Fatal("Cannot find certbot: ", err)
	}

	// Get certbot registered and ready to go.
	cmd := exec.Command("sh", "-c", "if [ -d ./config ]; then true; else certbot register --agree-tos --email jeff.allen@geneva.msf.org --config-dir ./config --work-dir ./work --logs-dir ./logs --non-interactive; fi")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		log.Fatal("could not initialize certbot: ", err)
	}

	ac := &acme.Client{
		DirectoryURL: dirUrl,
	}
	mgr = &autocert.Manager{
		Client:     ac,
		Prompt:     autocert.AcceptTOS,
		HostPolicy: HostWhitelistByDomains(domainNames),
		Cache:      autocert.DirCache(*cacheDir),
	}

	// Set up the server:
	// - listen on the correct IP address and port 443
	// - use autocert
	// - answer to /get-cert-from-csr
	// - return a generic page for other requests (depending on requested host)
	s := &http.Server{
		Addr: fmt.Sprintf("%v:443", certomatFqdn),
		TLSConfig: &tls.Config{
			GetCertificate: mgr.GetCertificate,
		},
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		log.Print(*r)

		if r.Host == certomatFqdn {
			w.Write(welcomeCertomat)
		} else {
			w.Write(welcomeOther)
		}
		return
	})
	http.Handle("/get-cert-from-csr",
		http.StripPrefix("/get-cert-from-csr",
			http.HandlerFunc(getHandler)))

	log.Print("Listening on ", s.Addr)
	err = s.ListenAndServeTLS("", "")
	if err != nil {
		log.Fatal(err)
	}
}

// Some text to put on the front page.
var welcomeCertomat = []byte("<h1>Certomat</h1><p>For more info see <a href=\"https://wiki.unifield.org/doku.php?id=infrastructure:internal:certomat\">the wiki</a>.</p>")

var welcomeOther = []byte("<h1>Local DNS is not configured</h1><p>Your Unifield server has sucessfully fetched a TLS certificate, but your local DNS is not redirecting you to the local IP address for the server. For more info see section 1.9 of the Unifield IT manual.</p>")
