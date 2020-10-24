package main

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
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
	Price       float64
	Ratio       float64
	Quantity    int
	URL         string
	NoQuantity  bool
	ShowDirect  bool
}

func Search(w http.ResponseWriter, r *http.Request) {
	sig := r.FormValue("sig")

	pageVars := genPageNav("Search", sig)

	if !DatabaseLoaded {
		pageVars.Title = "Great things are coming"
		pageVars.ErrorMessage = "Website is starting, please try again in a few minutes"

		render(w, "search.html", pageVars)
		return
	}

	searchParam, _ := GetParamFromSig(sig, "Search")
	canSearch, _ := strconv.ParseBool(searchParam)
	if SigCheck && !canSearch {
		pageVars.Title = "This feature is BANned"
		pageVars.ErrorMessage = ErrMsgPlus
		pageVars.ShowPromo = true

		render(w, "search.html", pageVars)
		return
	}

	var blocklist []string
	blocklistOpt, _ := GetParamFromSig(sig, "SearchDisabled")
	if blocklistOpt == "DEFAULT" || blocklistOpt == "" {
		blocklist = Config.SearchBlockList
	} else if blocklistOpt != "NONE" {
		blocklist = strings.Split(blocklistOpt, ",")
	}

	query := r.FormValue("q")
	if len(query) > MaxSearchQueryLen {
		pageVars.ErrorMessage = TooLongMessage

		render(w, "search.html", pageVars)
		return
	}

	bestSorting, _ := strconv.ParseBool(r.FormValue("b"))

	// Query is not null, let's get processing
	if query == "" {
		render(w, "search.html", pageVars)
		return
	}
	log.Println(query)

	// Keep track of what was searched
	pageVars.SearchQuery = query
	// Setup conditions keys, all etnries, and images
	pageVars.CondKeys = []string{"INDEX", "NM", "SP", "MP", "HP", "PO"}
	pageVars.Metadata = map[string]GenericCard{}

	// SEARCH
	foundSellers, foundVendors, tooMany := search(query, blocklist)

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
			pageVars.Metadata[cardId] = uuid2card(cardId, false)
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
			pageVars.Metadata[cardId] = uuid2card(cardId, false)
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
	if bestSorting {
		for cardId := range foundSellers {
			for cond := range foundSellers[cardId] {
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

	pageVars.FoundSellers = foundSellers
	pageVars.FoundVendors = foundVendors
	pageVars.SellerKeys = sortedKeysSeller
	pageVars.VendorKeys = sortedKeysVendor

	var source string
	u, err := url.Parse(r.Referer())
	if err != nil {
		log.Println(err)
		source = "n/a"
	} else {
		source = u.Path
	}
	Notify("search", fmt.Sprintf("[%s] from %s", query, source))

	render(w, "search.html", pageVars)
}

func search(query string, blocklist []string) (
	foundSellers map[string]map[string][]SearchEntry,
	foundVendors map[string][]SearchEntry,
	tooMany bool) {
	// Allocate memory
	foundSellers = map[string]map[string][]SearchEntry{}
	foundVendors = map[string][]SearchEntry{}

	// Set which comparison function to use depending on the search syntax
	cmpFunc := mtgmatcher.Equals

	// Filter out any element from the search syntax
	filterEdition := ""
	filterCondition := ""
	filterFoil := ""
	filterNumber := ""

	// Iterate over the various possible filters
	for _, tag := range []string{"s:", "c:", "f:", "sm:", "cn:"} {
		if strings.Contains(query, tag) {
			fields := strings.Fields(query)
			for _, field := range fields {
				if strings.HasPrefix(field, tag) {
					query = strings.Replace(query, field, "", 1)
					query = strings.TrimSpace(query)

					code := strings.TrimPrefix(field, tag)
					switch tag {
					case "s:":
						filterEdition = strings.ToUpper(code)
						break
					case "c:":
						filterCondition = code
						break
					case "cn:":
						filterNumber = code
						break
					case "f:":
						filterFoil = code
						if filterFoil == "yes" || filterFoil == "y" {
							filterFoil = "true"
						} else if filterFoil == "no" || filterFoil == "n" {
							filterFoil = "false"
						}
						break
					case "sm:":
						switch code {
						case "exact":
							cmpFunc = mtgmatcher.Equals
						case "prefix":
							cmpFunc = mtgmatcher.HasPrefix
						case "any":
							cmpFunc = mtgmatcher.Contains
						}
						break
					}
				}
			}
		}
	}

	// Support Scryfall bot syntax
	if strings.Contains(query, "|") {
		elements := strings.Split(query, "|")
		query = elements[0]
		if len(elements) > 1 {
			filterEdition = strings.TrimSpace(strings.ToUpper(elements[1]))
		}
		if len(elements) > 2 {
			filterNumber = strings.TrimSpace(elements[2])
		}
		cmpFunc = mtgmatcher.Equals
	} else {
		// Also support our own ID style
		card, err := mtgmatcher.GetUUID(strings.TrimSpace(query))
		if err == nil {
			query = card.Name
			filterEdition = card.SetCode
			filterNumber = card.Number
			filterFoil = fmt.Sprint(card.Foil)
			cmpFunc = mtgmatcher.Equals
		}
	}

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

		// Get inventory
		inventory, err := seller.Inventory()
		if err != nil {
			log.Println(err)
			continue
		}

		// Loop through cards
		for cardId, entries := range inventory {
			co, err := mtgmatcher.GetUUID(cardId)
			if err != nil {
				continue
			}

			// Run the comparison function set above
			if cmpFunc(co.Card.Name, query) {
				// Skip cards that are not of the desired set
				if filterEdition != "" {
					filters := strings.Split(filterEdition, ",")
					if !SliceStringHas(filters, co.Card.SetCode) {
						continue
					}
				}
				// Skip cards that are not of the desired collector number
				if filterNumber != "" {
					filters := strings.Split(filterNumber, ",")
					if !SliceStringHas(filters, co.Card.Number) {
						continue
					}
				}
				// Skip cards that are not as desired foil
				if filterFoil != "" {
					foilStatus, err := strconv.ParseBool(filterFoil)
					if err == nil {
						if foilStatus && !co.Foil {
							continue
						} else if !foilStatus && co.Foil {
							continue
						}
					}
				}

				// Loop thorugh available conditions
				for _, entry := range entries {
					// Skip cards that have not the desired condition
					if filterCondition != "" {
						filters := strings.Split(filterCondition, ",")
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

					// Only add Poor prices if there are no NM entries
					if conditions == "PO" && len(foundSellers[cardId]["NM"]) != 0 {
						continue
					}

					// Check if the current entry has any condition
					_, found = foundSellers[cardId][conditions]
					if !found {
						foundSellers[cardId][conditions] = []SearchEntry{}
					}

					name := seller.Info().Name
					if name == TCG_MAIN {
						name = "TCG Player (+s&h)"
					}
					// Prepare all the deets
					res := SearchEntry{
						ScraperName: name,
						Price:       entry.Price,
						Quantity:    entry.Quantity,
						URL:         entry.URL,
						NoQuantity:  seller.Info().NoQuantityInventory || seller.Info().MetadataOnly,
						ShowDirect:  seller.Info().Name == TCG_DIRECT,
					}
					if seller.Info().CountryFlag != "" {
						res.ScraperName += " " + seller.Info().CountryFlag
					}

					// Touchdown
					foundSellers[cardId][conditions] = append(foundSellers[cardId][conditions], res)
				}
			}
		}
	}

	// Really same as above
	for i, vendor := range Vendors {
		if vendor == nil {
			log.Println("nil vendor at position", i)
			continue
		}

		if SliceStringHas(blocklist, vendor.Info().Shorthand) {
			continue
		}

		buylist, err := vendor.Buylist()
		if err != nil {
			log.Println(err)
			continue
		}
		for cardId, blEntries := range buylist {
			co, err := mtgmatcher.GetUUID(cardId)
			if err != nil {
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

			if filterEdition != "" {
				filters := strings.Split(filterEdition, ",")
				if !SliceStringHas(filters, co.Card.SetCode) {
					continue
				}
			}
			if filterNumber != "" {
				filters := strings.Split(filterNumber, ",")
				if !SliceStringHas(filters, co.Card.Number) {
					continue
				}
			}
			if filterFoil != "" {
				foilStatus, err := strconv.ParseBool(filterFoil)
				if err == nil {
					if foilStatus && !co.Foil {
						continue
					} else if !foilStatus && co.Foil {
						continue
					}
				}
			}

			if cmpFunc(co.Card.Name, query) {
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
					name = "TCG Player Trade-In"
				}
				res := SearchEntry{
					ScraperName: name,
					Price:       entry.BuyPrice,
					Ratio:       entry.PriceRatio,
					Quantity:    entry.Quantity,
					URL:         entry.URL,
				}
				if vendor.Info().CountryFlag != "" {
					res.ScraperName += " " + vendor.Info().CountryFlag
				}
				foundVendors[cardId] = append(foundVendors[cardId], res)
			}
		}
	}

	return
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
