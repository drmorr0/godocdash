package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

const splitter = "=========================================================\n"
const insertSQL = "INSERT OR IGNORE INTO searchIndex(name, type, path) VALUES (?,?,?)"

var silent bool
var docsetDir string
var goRoot string

func main() {

	{
		flag.Bool("options.silent", false, "Silent mode (only print error)")
		flag.String("docset.name", "GoDoc", "Set docset name")
		flag.String("docset.icon", "", "Docset icon .png path")
		flag.String("docset.output", "", "Output path to store the docset, e.g. /tmp")
		flag.String("go.goroot", "", "Override goroot")
		cmdlineFilters := flag.String("docset.filters", "", "Comma separated filters, e.g. github.com/user/pkg1,user/pkg2")

		pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
		pflag.Parse()
		if err := viper.BindPFlags(pflag.CommandLine); err != nil {
			panic(fmt.Errorf("Fatal error config input: %s \n", err))
		}

		if cmdlineFilters != nil && *cmdlineFilters != "" {
			docsetFilters := strings.Split(*cmdlineFilters, ",")
			viper.Set("Docset.filters", docsetFilters)
		}

		viper.SetConfigName("godocset-config")
		viper.AddConfigPath("/tmp")
		viper.AddConfigPath(".")
		err := viper.ReadInConfig()
		if err != nil {
			panic(fmt.Errorf("Fatal error config file: %s \n", err))
		}
	}

	silent = viper.GetBool("Options.silent")
	name := viper.GetString("Docset.name")
	icon := viper.GetString("Docset.icon")
	output := viper.GetString("Docset.output")
	filter := viper.GetStringSlice("Docset.filters")
	goroot := viper.GetString("Go.goroot")

	if output != "" && !strings.HasSuffix(output, "/") {
		output = output + "/"
	}
	docsetDir = output + name + ".docset"

	if goroot != "" {
		goRoot = "-goroot=" + goroot
	}

	// icon
	err := writeIcon(icon)
	if err != nil {
		fmt.Println(err)
		return
	}

	// plist
	err = genPlist(name)
	if err != nil {
		fmt.Println(err)
		return
	}

	// DB
	db, err := createDB()
	if err != nil {
		fmt.Println(err)
		return
	}
	defer db.Close()

	// godoc
	cmd, host, err := runGodoc()
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() {
		printf("killing godoc on %s\n", host)
		err = cmd.Process.Kill()
		if err != nil {
			fmt.Printf("error killing godoc on %s: %s\n", host, err.Error())
		}
	}()

	// get package list
	packages, err := getPackages(host, filter)
	if err != nil {
		fmt.Println(err)
		return
	}

	// download static resources like css and js
	grabLib(host)

	// prepare
	stmt, err := db.Prepare(insertSQL)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer stmt.Close()
	// transaction
	tx, err := db.Begin()
	if err != nil {
		fmt.Println(err)
		return
	}
	defer tx.Commit()

	// download pages and insert DB indexes
	grabPackages(tx.Stmt(stmt), host, packages)
}

func writeIcon(p string) (err error) {
	var r io.Reader
	if p == "" {
		var buf []byte
		buf, err = Asset("asset/godoc.png")
		if err != nil {
			return
		}
		r = bytes.NewReader(buf)
	} else {
		var f *os.File
		f, err = os.Open(p)
		if err != nil {
			return
		}
		defer f.Close()
		r = bufio.NewReader(f)
	}

	outputPath := filepath.Join(docsetDir, "icon.png")
	err = os.MkdirAll(filepath.Dir(outputPath), 0755)
	if err != nil {
		return
	}
	w, err := os.Create(outputPath)
	if err != nil {
		return
	}
	_, err = io.Copy(w, r)
	return
}

func createDB() (db *sql.DB, err error) {
	p := filepath.Join(getResourcesDir(), "docSet.dsidx")
	err = os.MkdirAll(filepath.Dir(p), 0755)
	if err != nil {
		return
	}
	os.Remove(p)
	db, err = sql.Open("sqlite3", p)
	if err != nil {
		return db, err
	}

	_, err = db.Exec("CREATE TABLE searchIndex(id INTEGER PRIMARY KEY, name TEXT, type TEXT, path TEXT)")
	if err != nil {
		return
	}

	_, err = db.Exec("CREATE UNIQUE INDEX anchor ON searchIndex (name, type, path)")
	if err != nil {
		return
	}

	return
}

func runGodoc() (cmd *exec.Cmd, host string, err error) {
	// get a free port
	l, err := net.Listen("tcp", "")
	if err != nil {
		return
	}
	addr := l.Addr()
	l.Close()
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		err = errors.New("failed to find a free port: " + addr.String())
		return
	}

	// try running godoc on this port
	tryHost := "localhost:" + strconv.Itoa(tcpAddr.Port)
	cmd = exec.Command("godoc", "-http="+tryHost)
	if goRoot != "" {
		cmd.Args = append(cmd.Args, goRoot)
	}
	if !silent {
		cmd.Stderr = os.Stderr
		cmd.Stdout = os.Stdout
	}
	cmd.Env = os.Environ()
	err = cmd.Start()
	if err != nil {
		return
	}
	host = "http://" + tryHost

	// check port is valid now
	// godoc server has to index data, it needs some time to boot up
	// check pkg page is ready now
	for i := 0; i < 15; i++ {
		time.Sleep(500 * time.Millisecond)
		doc, err := goquery.NewDocument(host + "/pkg/")
		if err == nil {
			// "Scan is not yet complete. Please retry after a few moments"
			if doc.Find("span.alert").Size() == 0 {
				break
			}
		}
	}
	return
}

func getPackages(host string, filter []string) (packages []string, err error) {
	doc, err := goquery.NewDocument(host + "/pkg/")
	if err != nil {
		return
	}
	doc.Find("div.pkg-dir td.pkg-name a").Each(func(index int, pkg *goquery.Selection) {
		packageName, ok := pkg.Attr("href")
		if !ok {
			return
		}

		// ignore standard packages as there's official go docset already
		if strings.Contains(packageName, ".") && matchFilter(packageName, filter) {
			packages = append(packages, packageName)
		}
	})
	return
}

func matchFilter(keyword string, filter []string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, f := range filter {
		if strings.HasSuffix(f, "*") {
			wildcard := strings.TrimSuffix(strings.TrimPrefix(f, "github.com/"), "*")
			if strings.Contains(keyword, wildcard) {
				return true
			}
		}
		if strings.Contains(keyword, f) {
			return true
		}
	}
	return false
}

func grabPackages(stmt *sql.Stmt, host string, packages []string) {
	wg := &sync.WaitGroup{}
	total := 0
	for _, packageName := range packages {
		wg.Add(1)
		total += 1
		go grabPackage(
			wg,
			stmt,
			strings.TrimRight(packageName, "/"),
			host+"/pkg/"+packageName,
		)
		if total >= 10 {
			wg.Wait()
			total = 0
		}
	}

	wg.Wait()
	return
}

func grabPackage(wg *sync.WaitGroup, stmt *sql.Stmt, packageName string, url string) {
	var info *packageInfo
	for i := 0; i < 5; i++ {
		if info = tryGrabPackage(stmt, packageName, url); info.Err != nil {
			time.Sleep(time.Second * 2)
		} else {
			break
		}
	}
	info.Print()
	defer wg.Done()
}

func tryGrabPackage(stmt *sql.Stmt, packageName string, url string) *packageInfo {
	info := &packageInfo{Name: packageName}

	infoError := func(err error) *packageInfo {
		info.Err = err
		return info
	}

	resp, err := http.Get(url)
	if err != nil {
		return infoError(err)
	}
	defer resp.Body.Close()
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return infoError(err)
	}
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(buf))
	if err != nil {
		return infoError(err)
	}

	// skip directories
	pkgDir := doc.Find("h1").First()
	if !strings.HasPrefix(strings.TrimSpace(pkgDir.Text()), "Package") {
		return infoError(err)
	}
	info.IsPackage = true

	documentPath := getDocumentPath(info.Name)
	replaceLinks(doc, documentPath)
	newHTML, err := goquery.OuterHtml(doc.Selection)
	if err != nil {
		return infoError(err)
	}

	err = writeFile(documentPath, strings.NewReader(newHTML))
	if err != nil {
		return infoError(err)
	}

	info.Parse(doc)
	err = info.WriteInsert(stmt)

	return info
}

func grabLib(host string) {
	wg := &sync.WaitGroup{}
	wg.Add(1)
	grabDirectory(wg, host, "lib/godoc/")
	wg.Wait()
	return
}

func grabDirectory(wg *sync.WaitGroup, host string, relPath string) {
	defer wg.Done()

	resp, err := http.Get(host + "/" + relPath)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer resp.Body.Close()
	buf, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		fmt.Println(err)
		return
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(buf))
	if err != nil {
		fmt.Println(err)
		return
	}
	doc.Find("tbody tr").Each(func(index int, selection *goquery.Selection) {
		// skip ".."
		if len(selection.Children().Nodes) < 2 {
			return
		}
		href, ok := selection.Find("a").First().Attr("href")
		if !ok {
			return
		}

		url := host + "/" + relPath + href
		// download css and js
		if strings.HasSuffix(href, ".css") || strings.HasSuffix(href, ".js") {
			res, err := http.Get(url)
			if err != nil {
				fmt.Println(err)
			}
			defer res.Body.Close()
			err = writeFile(relPath+href, res.Body)
			if err != nil {
				fmt.Println(err)
			}
			return
		}
		// or walk into next directory
		wg.Add(1)
		go grabDirectory(wg, host, url)
	})
	return
}

func genPlist(docsetName string) (err error) {
	contentsDir := getContentsDir()
	err = os.MkdirAll(contentsDir, 0755)
	if err != nil {
		return
	}

	f, err := os.Create(filepath.Join(contentsDir, "Info.plist"))
	if err != nil {
		return
	}
	defer f.Close()
	titleName := strings.ToTitle(docsetName[0:1]) + docsetName[1:]
	f.WriteString(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleIdentifier</key>
	<string>%s</string>
	<key>CFBundleName</key>
	<string>%s</string>
	<key>DocSetPlatformFamily</key>
	<string>%s</string>
	<key>isDashDocset</key>
	<true/>
</dict>
</plist>`,
		docsetName,
		titleName,
		docsetName,
	))
	return
}

func replaceLinks(doc *goquery.Document, documentPath string) {
	dir := path.Dir(documentPath)

	// css
	doc.Find("link").Each(func(index int, selection *goquery.Selection) {
		href, ok := selection.Attr("href")
		if !ok {
			return
		}
		if !strings.HasSuffix(href, ".css") {
			return
		}
		newHref, err := filepath.Rel(dir, strings.TrimLeft(href, "/"))
		if err != nil {
			fmt.Println(err)
			return
		}
		selection.SetAttr("href", newHref)
	})

	// js
	doc.Find("script").Each(func(index int, selection *goquery.Selection) {
		src, ok := selection.Attr("src")
		if !ok {
			return
		}
		if !strings.HasSuffix(src, ".js") {
			return
		}
		newSrc, err := filepath.Rel(dir, strings.TrimLeft(src, "/"))
		if err != nil {
			fmt.Println(err)
			return
		}
		selection.SetAttr("src", newSrc)
	})
}

func writeFile(relPath string, r io.Reader) (err error) {
	p := filepath.Join(getResourcesDir(), "Documents", relPath)
	err = os.MkdirAll(filepath.Dir(p), 0755)
	if err != nil {
		return
	}

	f, err := os.Create(p)
	if err != nil {
		return
	}
	defer f.Close()

	_, err = io.Copy(f, r)
	return
}

func getResourcesDir() string {
	return filepath.Join(getContentsDir(), "Resources")
}

func getContentsDir() string {
	return filepath.Join(docsetDir, "Contents")
}

func getDocumentPath(packageName string) string {
	return path.Join("pkg", packageName, "index.html")
}

func printf(format string, a ...interface{}) {
	if !silent {
		fmt.Printf(format, a...)
	}
}
