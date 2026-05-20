package resolver

// exchangeToCountry maps canonical exchange codes to their 2-letter country code.
// Mirrored from apps/web/lib/exchange-constants.ts; drift guard in TestExchangeMappings.
var exchangeToCountry = map[string]string{
	"TSX": "CA", "TSV": "CA", "CSE": "CA", "NEO": "CA",
	"NGS": "US", "NSD": "US", "NSC": "US", "NYE": "US", "AMX": "US",
}

// exchangeDisplayNames maps canonical exchange codes to human-readable names.
// Mirrored from apps/web/lib/exchange-constants.ts; drift guard in TestExchangeMappings.
var exchangeDisplayNames = map[string]string{
	"TSX": "TSX", "TSV": "TSX-V", "CSE": "CSE", "NEO": "NEO",
	"NGS": "NASDAQ GS", "NSD": "NASDAQ GM", "NSC": "NASDAQ CM",
	"NYE": "NYSE", "AMX": "NYSE American",
}

// exchangesByCountry is the inverse of exchangeToCountry, precomputed for filtering.
var exchangesByCountry = map[string][]string{
	"CA": {"TSX", "TSV", "CSE", "NEO"},
	"US": {"NGS", "NSD", "NSC", "NYE", "AMX"},
}

// CountryFor returns "CA", "US", or "" for an exchange code.
func CountryFor(exchange string) string {
	return exchangeToCountry[exchange]
}

// DisplayNameFor returns the human-readable exchange name, or the code itself if unknown.
func DisplayNameFor(exchange string) string {
	if name, ok := exchangeDisplayNames[exchange]; ok {
		return name
	}
	return exchange
}

// ExchangesForCountry returns the exchange codes for a given country ("CA" or "US").
func ExchangesForCountry(country string) []string {
	return exchangesByCountry[country]
}
