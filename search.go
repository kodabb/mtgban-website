package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kodabb/go-mtgban/mtgmatcher"
)

const (
	MaxSearchQueryLen = 200
	MaxSearchResults  = 64
	TooLongMessage    = "Your query planeswalked away, try a shorter one"
	TooManyMessage    = "More results available, try adjusting your filters"
	NoResultsMessage  = "No results found"
)

type SearchEntry struct {
	ScraperName string
	Shorthand   string
	Price       float64
	Ratio       float64
	Quantity    int
	URL         string
	NoQuantity  bool
	ShowDirect  bool

	Country string

	IndexCombined bool
	Secondary     float64
}

var re = regexp.MustCompile(`(s|c|f|sm|cn|vndr|r|all|m):(("([^"]+)"|\S+))+`)

func Search(w http.ResponseWriter, r *http.Request) {
	sig := getSignatureFromCookies(r)

	pageVars := genPageNav("Search", sig)

	blocklistRetail, blocklistBuylist := getDefaultBlocklists(sig)

	query := r.FormValue("q")

	canSealed, _ := strconv.ParseBool(GetParamFromSig(sig, "SearchSealed"))
	canSealed = canSealed || (DevMode && !SigCheck)

	if canSealed {
		mode := r.FormValue("mode")

		pageVars.Nav = insertNavBar("Search", pageVars.Nav, []NavElem{
			NavElem{
				Name:   "Sealed",
				Short:  "🧱",
				Link:   "/search?mode=sealed",
				Active: mode == "sealed" || strings.Contains(query, "m:sealed"),
				Class:  "selected",
			},
		})

		if mode == "sealed" {
			pageVars.EditionSort = SealedEditionsSorted
			pageVars.EditionList = SealedEditionsList
			render(w, "product.html", pageVars)
			return
		}
	}

	if len(query) > MaxSearchQueryLen {
		pageVars.ErrorMessage = TooLongMessage

		render(w, "search.html", pageVars)
		return
	}

	chartId := r.FormValue("chart")
	// Check if query is a valid ID
	co, err := mtgmatcher.GetUUID(chartId)
	if err != nil {
		chartId = ""
	} else {
		// Override the query when chart is requested
		query = chartId
	}

	// If query is empty there is nothing to do
	if query == "" {
		render(w, "search.html", pageVars)
		return
	}

	// Keep track of what was searched
	pageVars.SearchQuery = query
	pageVars.SearchBest = readSetFlag(w, r, "b", "MTGBANSearchPref")
	// Setup conditions keys, all etnries, and images
	pageVars.CondKeys = []string{"INDEX", "NM", "SP", "MP", "HP", "PO"}
	pageVars.Metadata = map[string]GenericCard{}

	// SEARCH
	cleanQuery, options := parseSearchOptions(query)
	foundSellers, foundVendors, tooMany := searchParallel(cleanQuery, options, blocklistRetail, blocklistBuylist)

	pageVars.IsSealed = options["mode"] == "sealed"

	// Display a message if there are too many entries
	if tooMany {
		pageVars.InfoMessage = TooManyMessage
	}

	// Early exit if there no matches are found
	if len(foundSellers) == 0 && len(foundVendors) == 0 {
		pageVars.InfoMessage = NoResultsMessage
		render(w, "search.html", pageVars)
		return
	}

	// Make a cardId arrays so that they can be sorted later
	sortedKeysSeller := make([]string, 0, len(foundSellers))
	sortedKeysVendor := make([]string, 0, len(foundVendors))

	// Load up image links
	for cardId := range foundSellers {
		_, found := pageVars.Metadata[cardId]
		if !found {
			pageVars.Metadata[cardId] = uuid2card(cardId, false, true)
		}
		if pageVars.Metadata[cardId].Reserved {
			pageVars.HasReserved = true
		}
		if pageVars.Metadata[cardId].Stocks {
			pageVars.HasStocks = true
		}
		sortedKeysSeller = append(sortedKeysSeller, cardId)
	}
	for cardId := range foundVendors {
		_, found := pageVars.Metadata[cardId]
		if !found {
			pageVars.Metadata[cardId] = uuid2card(cardId, false, true)
		}
		if pageVars.Metadata[cardId].Reserved {
			pageVars.HasReserved = true
		}
		if pageVars.Metadata[cardId].Stocks {
			pageVars.HasStocks = true
		}
		sortedKeysVendor = append(sortedKeysVendor, cardId)
	}

	// Sort keys according to the sortSets() function, chronologically
	sort.Slice(sortedKeysSeller, func(i, j int) bool {
		return sortSets(sortedKeysSeller[i], sortedKeysSeller[j])
	})
	sort.Slice(sortedKeysVendor, func(i, j int) bool {
		return sortSets(sortedKeysVendor[i], sortedKeysVendor[j])
	})

	// Optionally sort according to price
	if pageVars.SearchBest {
		for cardId := range foundSellers {
			for cond := range foundSellers[cardId] {
				// These entries are special, do not sort them
				if cond == "INDEX" {
					continue
				}
				sort.Slice(foundSellers[cardId][cond], func(i, j int) bool {
					return foundSellers[cardId][cond][i].Price < foundSellers[cardId][cond][j].Price
				})
			}
		}
		for cardId := range foundVendors {
			sort.Slice(foundVendors[cardId], func(i, j int) bool {
				return foundVendors[cardId][i].Price > foundVendors[cardId][j].Price
			})
		}
	}

	// Readjust array of INDEX entires
	for cardId := range foundSellers {
		indexArray := foundSellers[cardId]["INDEX"]
		tmp := indexArray[:0]
		mkmIndex := -1
		tcgIndex := -1

		// Iterate on array, always passthrough, except for specific entries
		for i := range indexArray {
			switch indexArray[i].ScraperName {
			case MKM_LOW:
				// Save reference to the array
				tmp = append(tmp, indexArray[i])
				mkmIndex = len(tmp) - 1
			case MKM_TREND:
				// If the reference is found, add a secondary price
				// otherwise just leave it as is
				if mkmIndex >= 0 {
					tmp[mkmIndex].Secondary = indexArray[i].Price
					tmp[mkmIndex].ScraperName = "MKM (Low / Trend)"
					tmp[mkmIndex].IndexCombined = true
				} else {
					tmp = append(tmp, indexArray[i])
				}
			case TCG_LOW:
				// Save reference to the array
				tmp = append(tmp, indexArray[i])
				tcgIndex = len(tmp) - 1
			case TCG_MARKET:
				// If the reference is found, add a secondary price
				// otherwise just leave it as is
				if tcgIndex >= 0 {
					tmp[tcgIndex].Secondary = indexArray[i].Price
					tmp[tcgIndex].ScraperName = "TCG (Low / Market)"
					tmp[tcgIndex].IndexCombined = true
				} else {
					tmp = append(tmp, indexArray[i])
				}
			default:
				tmp = append(tmp, indexArray[i])
			}
		}

		foundSellers[cardId]["INDEX"] = tmp
	}

	pageVars.FoundSellers = foundSellers
	pageVars.FoundVendors = foundVendors
	pageVars.SellerKeys = sortedKeysSeller
	pageVars.VendorKeys = sortedKeysVendor

	// CHART ALL THE THINGS
	if chartId != "" {
		// Rebuild the original query
		searchQuery := cleanQuery
		if options["edition"] != "" {
			searchQuery += " s:" + options["edition"]
		}
		if options["number"] != "" {
			searchQuery += " cn:" + options["number"]
		}
		if options["foil"] != "" {
			searchQuery += " f:" + options["foil"]
		}
		pageVars.SearchQuery = searchQuery

		// Retrieve data
		labels, err := getDateAxisValues(chartId)
		if err != nil {
			pageVars.InfoMessage = "No chart data available"
		} else {
			pageVars.AxisLabels = labels
			pageVars.ChartID = chartId

			for _, config := range enabledDatasets {
				if co.Sealed && !config.HasSealed {
					continue
				}
				dataset, err := getDataset(chartId, labels, config)
				if err != nil {
					log.Println(err)
					continue
				}
				pageVars.Datasets = append(pageVars.Datasets, dataset)
			}
		}

		altId, err := mtgmatcher.Match(&mtgmatcher.Card{
			Id:   chartId,
			Foil: !co.Foil,
		})
		if err == nil && altId != chartId {
			pageVars.Alternative = altId
		}

		pageVars.StocksURL = pageVars.Metadata[chartId].StocksURL
	} else {
		// Display tracking for non-chart requests
		var source string
		utm := r.FormValue("utm_source")
		if utm == "banbot" {
			id := r.FormValue("utm_affiliate")
			source = fmt.Sprintf("banbot (%s)", id)
		} else if utm == "autocard" {
			source = "autocard anywhere"
		} else {
			u, err := url.Parse(r.Referer())
			if err != nil {
				log.Println(err)
				source = "n/a"
			} else {
				if strings.Contains(u.Host, "mtgban") {
					source = u.Path
				} else {
					// Avoid automatic URL expansion in Discord
					source = fmt.Sprintf("<%s>", u.String())
				}
			}
		}
		user := GetParamFromSig(sig, "UserEmail")
		msg := fmt.Sprintf("[%s] from %s by %s", query, source, user)
		Notify("search", msg)
		LogPages["Search"].Println(msg)
	}

	render(w, "search.html", pageVars)
}

func parseSearchOptions(query string) (string, map[string]string) {
	// Filter out any element from the search syntax
	options := map[string]string{}

	// Optionally ignore all search options
	ignore := false

	// Iterate over the various possible filters
	fields := re.FindAllString(query, -1)
	for _, field := range fields {
		query = strings.Replace(query, field, "", 1)

		index := strings.Index(field, ":")
		code := field[index+1:]

		switch {
		case strings.HasPrefix(field, "s:"):
			options["edition"] = findEdition(code)
		case strings.HasPrefix(field, "c:"):
			options["condition"] = strings.ToUpper(code)
		case strings.HasPrefix(field, "cn:"):
			options["number"] = code
		case strings.HasPrefix(field, "r:"):
			options["rarity"] = strings.ToLower(code)
		case strings.HasPrefix(field, "f:"):
			val, _ := strconv.ParseBool(code)
			options["foil"] = "false"
			if val {
				options["foil"] = "true"
			}
		case strings.HasPrefix(field, "sm:"):
			options["search_mode"] = strings.ToLower(code)
		case strings.HasPrefix(field, "vndr:"):
			options["scraper"] = strings.ToUpper(code)
			// Hack to support the various subseller names of tcg
			if strings.Contains(options["scraper"], "TCG") {
				options["scraper"] = strings.Replace(options["scraper"], "TCG", "TCG Player,TCGMkt", 1)
			}
			// Hack to show all versions of a card
		case strings.HasPrefix(field, "all:"):
			ignore, _ = strconv.ParseBool(code)
		case strings.HasPrefix(field, "m:"):
			options["mode"] = code
		}
	}

	// Filter out the out of standard syntax
	if strings.HasSuffix(query, "&") {
		query = strings.TrimSuffix(query, "&")
		options["foil"] = "false"
	} else if strings.HasSuffix(query, "*") {
		query = strings.TrimSuffix(query, "*")
		options["foil"] = "true"
	}

	if strings.HasPrefix(query, "random") {
		edition := ""
		elements := strings.Split(query, "|")
		if len(elements) > 1 {
			edition = findEdition(elements[1])
		}

		sets := mtgmatcher.GetSets()
		for _, set := range sets {
			if edition != "" && !SliceStringHas(strings.Split(edition, ","), set.Code) {
				continue
			}
			if len(set.Cards) == 0 {
				continue
			}
			index := rand.Intn(len(set.Cards))
			query = set.Cards[index].UUID
			break
		}
	}

	// Support Scryfall bot syntax
	if strings.Contains(query, "|") {
		elements := strings.Split(query, "|")
		query = elements[0]
		if len(elements) > 1 {
			options["edition"] = findEdition(elements[1])
		}
		if len(elements) > 2 {
			options["number"] = strings.TrimSpace(elements[2])
		}
	} else {
		// Also support our own ID style
		card, err := mtgmatcher.GetUUID(strings.TrimSpace(query))
		if err == nil {
			query = card.Name
			options["edition"] = card.SetCode
			options["number"] = card.Number
			options["foil"] = fmt.Sprint(card.Foil)
			options["search_mode"] = "exact"
		}
	}

	// Hack to ignore all search options
	if ignore {
		return query, map[string]string{}
	}

	return query, options
}

// Return a comma-separated string of set codes, from a comma-separated
// list of codes or edition names. If no match is found, the input code
// segment is returned as-is.
func findEdition(code string) string {
	var out []string

	code = strings.TrimSpace(code)
	for _, field := range strings.Split(code, ",") {
		field = strings.TrimPrefix(field, "\"")
		field = strings.TrimSuffix(field, "\"")

		set, err := mtgmatcher.GetSet(field)
		if err == nil {
			out = append(out, set.Code)
			continue
		}
		set, err = mtgmatcher.GetSetByName(field)
		if err == nil {
			out = append(out, set.Code)
			continue
		}
		out = append(out, field)
	}
	return strings.Join(out, ",")
}

func mode2func(mode string) (out func(string, string) bool) {
	out = mtgmatcher.Equals
	switch mode {
	case "exact":
		out = mtgmatcher.Equals
	case "prefix":
		out = mtgmatcher.HasPrefix
	case "any":
		out = mtgmatcher.Contains
	}
	return
}

func shouldSkipCard(query, cardId string, options map[string]string) bool {
	co, err := mtgmatcher.GetUUID(cardId)
	if err != nil {
		return true
	}

	// Skip cards that are not from the desired mode
	if options["mode"] != "" {
		if (options["mode"] == "sealed" && !co.Sealed) ||
			(options["mode"] == "singles" && co.Sealed) {
			return true
		}
	}

	// Skip cards that are not of the desired set
	if options["edition"] != "" {
		filters := strings.Split(options["edition"], ",")
		if !SliceStringHas(filters, co.SetCode) {
			return true
		}
	}
	if options["not_edition"] != "" {
		filters := strings.Split(options["not_edition"], ",")
		if SliceStringHas(filters, co.SetCode) {
			return true
		}
	}

	// Skip cards that are not of the desired collector number
	if options["number"] != "" {
		filters := strings.Split(options["number"], ",")
		if !SliceStringHas(filters, co.Number) {
			return true
		}
	}

	// Skip cards that are not of the desired rarities
	if options["rarity"] != "" {
		filters := strings.Split(options["rarity"], ",")
		for _, rarity := range filters {
			if rarity != co.Rarity && !strings.HasPrefix(co.Rarity, rarity) {
				return true
			}
		}
	}

	// Skip cards that are not as desired foil
	if options["foil"] != "" {
		foilStatus, err := strconv.ParseBool(options["foil"])
		if err == nil {
			if foilStatus && !co.Foil {
				return true
			} else if !foilStatus && co.Foil {
				return true
			}
		}
	}

	// Run the comparison function to use depending on the search syntax
	cmpFunc := mode2func(options["search_mode"])
	if !cmpFunc(co.Name, query) {
		return true
	}

	return false
}

func searchSellers(query string, blocklist []string, options map[string]string) (foundSellers map[string]map[string][]SearchEntry, tooMany bool) {
	// Allocate memory
	foundSellers = map[string]map[string][]SearchEntry{}

	// Search sellers
	for i, seller := range Sellers {
		if seller == nil {
			log.Println("nil seller at position", i)
			continue
		}

		// Skip any seller explicitly in blocklist
		if SliceStringHas(blocklist, seller.Info().Shorthand) {
			continue
		}

		// Skip any unwanted sellers
		if options["scraper"] != "" {
			filters := strings.Split(options["scraper"], ",")
			if !SliceStringHas(filters, seller.Info().Shorthand) {
				continue
			}
		}

		// Skip sellers not from the desired mode
		if options["mode"] != "" {
			if (options["mode"] == "sealed" && !seller.Info().SealedMode) ||
				(options["mode"] == "singles" && seller.Info().SealedMode) {
				continue
			}
		}

		// Get inventory
		inventory, err := seller.Inventory()
		if err != nil {
			log.Println(err)
			continue
		}

		// Loop through cards
		for cardId, entries := range inventory {
			if shouldSkipCard(query, cardId, options) {
				continue
			}

			// Loop thorugh available conditions
			for _, entry := range entries {
				// Skip cards that have not the desired condition
				if options["condition"] != "" {
					filters := strings.Split(options["condition"], ",")
					if !SliceStringHas(filters, entry.Conditions) {
						continue
					}
				}

				// No price no dice
				if entry.Price == 0 {
					continue
				}

				// Check if card already has any entry
				_, found := foundSellers[cardId]
				if !found {
					// Skip when you have too many results
					if len(foundSellers) > MaxSearchResults {
						tooMany = true
						continue
					}
					foundSellers[cardId] = map[string][]SearchEntry{}
				}

				// Set conditions - handle the special TCG one that appears
				// at the top of the results
				conditions := entry.Conditions
				if seller.Info().MetadataOnly {
					conditions = "INDEX"
				}

				// Only add Poor prices if there are no NM and SP entries
				if conditions == "PO" && len(foundSellers[cardId]["NM"]) != 0 && len(foundSellers[cardId]["SP"]) != 0 {
					continue
				}

				// Check if the current entry has any condition
				_, found = foundSellers[cardId][conditions]
				if !found {
					foundSellers[cardId][conditions] = []SearchEntry{}
				}

				name := seller.Info().Name
				// Prepare all the deets
				res := SearchEntry{
					ScraperName: name,
					Shorthand:   seller.Info().Shorthand,
					Price:       entry.Price,
					Quantity:    entry.Quantity,
					URL:         entry.URL,
					NoQuantity:  seller.Info().NoQuantityInventory || seller.Info().MetadataOnly,
					ShowDirect:  seller.Info().Name == TCG_DIRECT,
					Country:     seller.Info().CountryFlag,
				}

				// Touchdown
				foundSellers[cardId][conditions] = append(foundSellers[cardId][conditions], res)
			}
		}
	}

	return
}

func searchVendors(query string, blocklist []string, options map[string]string) (foundVendors map[string][]SearchEntry, tooMany bool) {
	foundVendors = map[string][]SearchEntry{}

	for i, vendor := range Vendors {
		if vendor == nil {
			log.Println("nil vendor at position", i)
			continue
		}

		if SliceStringHas(blocklist, vendor.Info().Shorthand) {
			continue
		}

		if options["scraper"] != "" {
			filters := strings.Split(options["scraper"], ",")
			if !SliceStringHas(filters, vendor.Info().Shorthand) {
				continue
			}
		}

		if options["mode"] != "" {
			if (options["mode"] == "sealed" && !vendor.Info().SealedMode) ||
				(options["mode"] == "singles" && vendor.Info().SealedMode) {
				continue
			}
		}

		buylist, err := vendor.Buylist()
		if err != nil {
			log.Println(err)
			continue
		}
		for cardId, blEntries := range buylist {
			if shouldSkipCard(query, cardId, options) {
				continue
			}

			// Look up the NM printing
			nmIndex := 0
			if vendor.Info().MultiCondBuylist {
				for nmIndex = range blEntries {
					if blEntries[nmIndex].Conditions == "NM" {
						break
					}
				}
			}
			entry := blEntries[nmIndex]

			_, found := foundVendors[cardId]
			if !found {
				if len(foundVendors) > MaxSearchResults {
					tooMany = true
					continue
				}
				foundVendors[cardId] = []SearchEntry{}
			}
			name := vendor.Info().Name
			if name == "TCG Player Market" {
				name = "TCG Trade-In"
			}
			res := SearchEntry{
				ScraperName: name,
				Shorthand:   vendor.Info().Shorthand,
				Price:       entry.BuyPrice,
				Ratio:       entry.PriceRatio,
				Quantity:    entry.Quantity,
				URL:         entry.URL,
				Country:     vendor.Info().CountryFlag,
			}
			foundVendors[cardId] = append(foundVendors[cardId], res)
		}
	}

	return
}

func searchParallel(query string, options map[string]string, blocklistRetail, blocklistBuylist []string) (foundSellers map[string]map[string][]SearchEntry, foundVendors map[string][]SearchEntry, tooMany bool) {
	var manySellers, manyVendors bool
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		foundSellers, manySellers = searchSellers(query, blocklistRetail, options)
		wg.Done()
	}()
	go func() {
		foundVendors, manyVendors = searchVendors(query, blocklistBuylist, options)
		wg.Done()
	}()

	wg.Wait()

	// No results with exact? Let's try again with prefix
	// Not any because it's too imprescise, also we catch results with odd layouts
	if len(foundSellers) == 0 && len(foundVendors) == 0 &&
		(options["search_mode"] == "exact" || options["search_mode"] == "") {
		options["search_mode"] = "prefix"
		return searchParallel(query, options, blocklistRetail, blocklistBuylist)
	}

	return foundSellers, foundVendors, manySellers || manyVendors
}

func sortSets(uuidI, uuidJ string) bool {
	cI, err := mtgmatcher.GetUUID(uuidI)
	if err != nil {
		return false
	}
	setI, err := mtgmatcher.GetSet(cI.Card.SetCode)
	if err != nil {
		return false
	}
	dateI := setI.ReleaseDate
	if cI.Card.OriginalReleaseDate != "" {
		dateI = cI.Card.OriginalReleaseDate
	}
	setDateI, err := time.Parse("2006-01-02", dateI)
	if err != nil {
		return false
	}
	editionI := setI.Name

	cJ, err := mtgmatcher.GetUUID(uuidJ)
	if err != nil {
		return false
	}
	setJ, err := mtgmatcher.GetSet(cJ.Card.SetCode)
	if err != nil {
		return false
	}
	dateJ := setJ.ReleaseDate
	if cJ.Card.OriginalReleaseDate != "" {
		dateJ = cJ.Card.OriginalReleaseDate
	}
	setDateJ, err := time.Parse("2006-01-02", dateJ)
	if err != nil {
		return false
	}
	editionJ := setJ.Name

	// If the two sets have the same release date, let's dig more
	if setDateI.Equal(setDateJ) {
		// If they are part of the same edition, check for their collector number
		// taking their foiling into consideration
		if editionI == editionJ {
			// If their number is the same, check for foiling status
			if cI.Card.Number == cJ.Card.Number {
				if cI.Foil == true && cJ.Foil == false {
					return false
				} else if cI.Foil == false && cJ.Foil == true {
					return true
				}
			}

			// If both are foil or both are non-foil, check their number
			cInum, errI := strconv.Atoi(cI.Card.Number)
			cJnum, errJ := strconv.Atoi(cJ.Card.Number)
			if errI == nil && errJ == nil {
				return cInum < cJnum
			}
			// If either one is not a number (due to extra letters) just
			// do a normal string comparison
			return cI.Card.Number < cJ.Card.Number

			// For the special case of set promos, always keeps them after
		} else if setI.ParentCode == "" && setJ.ParentCode != "" {
			return true
		} else if setJ.ParentCode == "" && setI.ParentCode != "" {
			return false
		} else {
			return editionI < editionJ
		}
	}

	return setDateI.After(setDateJ)
}
