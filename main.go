package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"database/sql"

	_ "github.com/go-sql-driver/mysql"
	cron "gopkg.in/robfig/cron.v2"

	"github.com/kodabb/go-mtgban/mtgban"
	"github.com/kodabb/go-mtgban/mtgdb"
)

type NavElem struct {
	Active bool
	Class  string
	Link   string
	Name   string
	Short  string
}

type Arbitrage struct {
	Name       string
	LastUpdate string
	Arbit      []mtgban.ArbitEntry
	Len        int
	HasCredit  bool
}

type CardMeta struct {
	ImageURL     string
	KeyruneHTML  string
	KeyruneTitle string
}

type PageVars struct {
	Nav       []NavElem
	Signature string

	PatreonId    string
	PatreonURL   string
	PatreonLogin bool
	ShowPromo    bool

	PatreonPartnerId string

	Title        string
	ErrorMessage string
	InfoMessage  string
	LastUpdate   string

	SearchQuery  string
	CondKeys     []string
	FoundSellers map[mtgdb.Card]map[string][]mtgban.CombineEntry
	FoundVendors map[mtgdb.Card][]mtgban.CombineEntry
	Images       map[mtgdb.Card]string
	Metadata     map[mtgdb.Card]CardMeta

	SellerShort  string
	SellerFull   string
	SellerUpdate string

	Arb        []Arbitrage
	UseCredit  bool
	FilterCond bool
	FilterFoil bool
	FilterComm bool
	FilterNega bool
}

var DefaultNav = []NavElem{
	NavElem{
		Name:  "🏡 Home",
		Short: "🏡",
		Link:  "/",
	},
	NavElem{
		Name:  "🔍 Search",
		Short: "🔍",
		Link:  "/search",
	},
	NavElem{
		Name:  "📈 Arbitrage",
		Short: "📈",
		Link:  "arbit",
	},
}

var Config struct {
	Port           int               `json:"port"`
	DBAddress      string            `json:"db_address"`
	Affiliate      map[string]string `json:"affiliate"`
	Api            map[string]string `json:"api"`
	DefaultSellers []string          `json:"default_sellers"`
	Patreon        struct {
		Secret map[string]string   `json:"secret"`
		Ids    map[string][]string `json:"ids"`
	} `json:"patreon"`
}

var DevMode bool
var SigCheck bool
var LastUpdate time.Time
var DatabaseLoaded bool
var Sellers []mtgban.Seller
var Vendors []mtgban.Vendor
var CardDB *sql.DB

func Favicon(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "img/misc/favicon.ico")
}

// FileSystem custom file system handler
type FileSystem struct {
	httpfs http.FileSystem
}

// Open opens file
func (fs *FileSystem) Open(path string) (http.File, error) {
	f, err := fs.httpfs.Open(path)
	if err != nil {
		return nil, err
	}

	s, err := f.Stat()
	if s.IsDir() {
		index := strings.TrimSuffix(path, "/") + "/index.html"
		_, err := fs.httpfs.Open(index)
		if err != nil {
			return nil, err
		}
	}

	return f, nil
}

func genPageNav(activeTab, sig string) PageVars {
	exp, _ := GetParamFromSig(sig, "Expires")
	expires, _ := strconv.ParseInt(exp, 10, 64)
	msg := ""
	patreonLogin := false
	if expires < time.Now().Unix() {
		if sig != "" {
			msg = ErrMsgExpired
			patreonLogin = true
		}
	}

	pageVars := PageVars{
		Title:        "BAN " + activeTab,
		Signature:    sig,
		PatreonId:    PatreonClientId,
		PatreonURL:   PatreonHost,
		PatreonLogin: patreonLogin,
		ErrorMessage: msg,
		LastUpdate:   LastUpdate.Format(time.RFC3339),

		PatreonPartnerId: PatreonPartnerId,
	}
	pageVars.Nav = make([]NavElem, len(DefaultNav))
	copy(pageVars.Nav, DefaultNav)

	signature := ""
	if sig != "" {
		signature = "?sig=" + sig
	}

	mainNavIndex := 0
	for i := range pageVars.Nav {
		pageVars.Nav[i].Link += signature
		// Ingore the starting emoji
		if strings.HasSuffix(pageVars.Nav[i].Name, activeTab) {
			mainNavIndex = i
		}
	}
	pageVars.Nav[mainNavIndex].Active = true
	pageVars.Nav[mainNavIndex].Class = "active"
	return pageVars
}

func loadVars(cfg string) error {
	// Load from command line
	file, err := os.Open(cfg)
	if err != nil {
		return err
	}
	defer file.Close()

	d := json.NewDecoder(file)
	err = d.Decode(&Config)
	if err != nil {
		return err
	}

	// Load from env
	keyVars := []string{
		"BAN_SECRET",
	}
	for _, key := range keyVars {
		v := os.Getenv(key)
		if v == "" {
			return fmt.Errorf("%s variable not set", key)
		}
	}

	return nil
}

func main() {
	config := flag.String("cfg", "config.json", "Load configuration file")
	devMode := flag.Bool("dev", false, "Enable developer mode")
	sigCheck := flag.Bool("sig", false, "Enable signature verification")
	flag.Parse()
	DevMode = *devMode
	SigCheck = true
	if DevMode {
		SigCheck = *sigCheck
	}

	// load necessary environmental variables
	err := loadVars(*config)
	if err != nil {
		log.Fatalln(err)
	}

	CardDB, err = sql.Open("mysql", Config.DBAddress+"/mtgjson")
	if err != nil {
		log.Fatalln(err)
	}

	// load website up
	go func() {
		var err error

		if !DevMode {
			log.Println("Loading MTGJSONv5")
			err = loadDatastore()
			if err != nil {
				log.Fatalln(err)
			}
		}

		log.Println("Loading MTGJSON")
		err = loadDB()
		if err != nil {
			log.Fatalln(err)
		}

		loadScrapers(true, true)
		DatabaseLoaded = true

		// If today's cache is missing, schedule a refresh right away
		files, err := ioutil.ReadDir(fmt.Sprintf("cache_inv/%03d", time.Now().YearDay()))
		if err != nil || len(files) < len(Sellers) {
			log.Println("Loaded inventory data too old, refreshing in the background")
			loadScrapers(true, false)
		}
		files, err = ioutil.ReadDir(fmt.Sprintf("cache_bl/%03d", time.Now().YearDay()))
		if err != nil || len(files) < len(Vendors) {
			log.Println("Loaded buylist data too old, refreshing in the background")
			loadScrapers(false, true)
		}

		// Set up new refreshes as needed
		c := cron.New()
		// refresh every day at 13:00
		c.AddFunc("0 13 * * *", func() {
			loadScrapers(true, true)
		})
		// refresh CK at every 8th hour
		c.AddFunc("0 */8 * * *", loadCK)
		// refresh at 12:00 every Tuesday
		c.AddFunc("0 12 * * 2", func() {
			log.Println("Reloading MTGJSON")
			err := loadDB()
			if err != nil {
				log.Println(err)
			}
		})
		c.Start()
	}()

	// serve everything in known folders as a file
	http.Handle("/css/", http.StripPrefix("/css/", http.FileServer(&FileSystem{http.Dir("css")})))
	http.Handle("/img/", http.StripPrefix("/img/", http.FileServer(&FileSystem{http.Dir("img")})))
	http.Handle("/js/", http.StripPrefix("/js/", http.FileServer(&FileSystem{http.Dir("js")})))

	// when navigating to /home it should serve the home page
	http.Handle("/", noSigning(http.HandlerFunc(Home)))
	http.Handle("/search", enforceSigning(http.HandlerFunc(Search)))
	http.Handle("/arbit", enforceSigning(http.HandlerFunc(Arbit)))
	http.Handle("/api/mtgjson/ck.json", enforceSigning(http.HandlerFunc(API)))
	http.HandleFunc("/favicon.ico", Favicon)
	http.HandleFunc("/auth", Auth)
	log.Fatal(http.ListenAndServe(":"+fmt.Sprint(Config.Port), nil))
}

func render(w http.ResponseWriter, tmpl string, pageVars PageVars) {
	tmpl = fmt.Sprintf("templates/%s", tmpl) // prefix the name passed in with templates/

	t, err := template.ParseFiles(tmpl) // parse the template file held in the templates folder
	if err != nil {                     // if there is an error
		log.Print("template parsing error: ", err) // log it
		return
	}

	err = t.Execute(w, pageVars) // execute the template and pass in the variables to fill the gaps
	if err != nil {              // if there is an error
		log.Print("template executing error: ", err) //log it
	}
}
