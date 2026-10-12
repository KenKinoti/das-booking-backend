package barstock

// CatalogItem is one entry of the starter catalogue: generic items common in
// Kenyan bars with their sizes and packs. There are NO prices — every club
// buys and sells at its own prices.
type CatalogItem struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Category  string  `json:"category"`
	BaseUnit  string  `json:"base_unit"`
	UnitSize  float64 `json:"unit_size"`
	UnitLabel string  `json:"unit_label"`
	PackSize  int     `json:"pack_size"`
	PackLabel string  `json:"pack_label"`
	// MenuItems says what "make menu items" creates: tot + bottle for
	// spirits, the item itself (1:1) for bottles and cans sold as they are.
	MenuItems []string `json:"menu_items"`
}

func spirit(key, name string, ml float64, pack int) CatalogItem {
	return CatalogItem{Key: key, Name: name, Category: "spirits", BaseUnit: BaseML, UnitSize: ml, UnitLabel: "bottle", PackSize: pack, PackLabel: "case", MenuItems: []string{"tot", "bottle"}}
}

func counted(key, name, cat, label string, pack int, packLabel string, sold bool) CatalogItem {
	c := CatalogItem{Key: key, Name: name, Category: cat, BaseUnit: BaseUnit, UnitSize: 1, UnitLabel: label, PackSize: pack, PackLabel: packLabel, MenuItems: []string{}}
	if sold {
		c.MenuItems = []string{"self"}
	}
	return c
}

// Catalog is the starter list offered by GET /bar/setup/catalog.
var Catalog = []CatalogItem{
	// Beer — 500 ml bottles come in crates of 25, cans in trays of 24.
	counted("beer_tusker_lager_500", "Tusker Lager 500 ml", "beer", "bottle", 25, "crate", true),
	counted("beer_white_cap_500", "White Cap Lager 500 ml", "beer", "bottle", 25, "crate", true),
	counted("beer_guinness_500", "Guinness Foreign Extra Stout 500 ml", "beer", "bottle", 25, "crate", true),
	counted("beer_tusker_can_500", "Tusker Lager can 500 ml", "beer", "can", 24, "tray", true),
	counted("beer_guinness_can_500", "Guinness can 500 ml", "beer", "can", 24, "tray", true),
	counted("cider_savanna_330", "Savanna Dry Cider 330 ml", "cider", "bottle", 24, "case", true),
	// Spirits — poured by the tot, also sold by the bottle.
	spirit("spirit_jw_black_750", "Johnnie Walker Black Label 750 ml", 750, 12),
	spirit("spirit_jameson_750", "Jameson Irish Whiskey 750 ml", 750, 12),
	spirit("spirit_jameson_1l", "Jameson Irish Whiskey 1 L", 1000, 12),
	spirit("spirit_gilbeys_750", "Gilbey's Gin 750 ml", 750, 12),
	spirit("spirit_gilbeys_250", "Gilbey's Gin 250 ml", 250, 24),
	spirit("spirit_smirnoff_750", "Smirnoff Vodka 750 ml", 750, 12),
	spirit("spirit_smirnoff_250", "Smirnoff Vodka 250 ml", 250, 24),
	spirit("spirit_captain_morgan_750", "Captain Morgan Spiced Gold 750 ml", 750, 12),
	spirit("spirit_bacardi_750", "Bacardi Carta Blanca white rum 750 ml", 750, 12),
	spirit("spirit_viceroy_750", "Viceroy Brandy 750 ml", 750, 12),
	spirit("spirit_kenya_cane_750", "Kenya Cane 750 ml", 750, 12),
	spirit("spirit_amarula_750", "Amarula Cream Liqueur 750 ml", 750, 12),
	// Wine — poured or sold by the bottle.
	{Key: "wine_house_red_750", Name: "House red wine 750 ml", Category: "wine", BaseUnit: BaseML, UnitSize: 750, UnitLabel: "bottle", PackSize: 6, PackLabel: "case", MenuItems: []string{"bottle"}},
	{Key: "wine_house_white_750", Name: "House white wine 750 ml", Category: "wine", BaseUnit: BaseML, UnitSize: 750, UnitLabel: "bottle", PackSize: 6, PackLabel: "case", MenuItems: []string{"bottle"}},
	// Soft drinks, mixers, energy drinks, water.
	counted("soft_coke_300", "Coca-Cola 300 ml", "soft_drinks", "bottle", 24, "crate", true),
	counted("soft_coke_500", "Coca-Cola 500 ml", "soft_drinks", "bottle", 12, "pack", true),
	counted("soft_fanta_orange_300", "Fanta Orange 300 ml", "soft_drinks", "bottle", 24, "crate", true),
	counted("soft_stoney_300", "Stoney Tangawizi 300 ml", "soft_drinks", "bottle", 24, "crate", true),
	counted("mixer_tonic_300", "Schweppes Tonic Water 300 ml", "mixers", "bottle", 24, "crate", true),
	counted("mixer_soda_300", "Schweppes Soda Water 300 ml", "mixers", "bottle", 24, "crate", true),
	counted("mixer_bitter_lemon_300", "Krest Bitter Lemon 300 ml", "mixers", "bottle", 24, "crate", true),
	{Key: "mixer_lime_cordial_1l", Name: "Lime cordial 1 L", Category: "mixers", BaseUnit: BaseML, UnitSize: 1000, UnitLabel: "bottle", PackSize: 12, PackLabel: "case", MenuItems: []string{}},
	counted("energy_red_bull_250", "Red Bull 250 ml can", "energy", "can", 24, "tray", true),
	counted("energy_monster_500", "Monster Energy 500 ml can", "energy", "can", 24, "tray", true),
	counted("water_still_500", "Still water 500 ml", "water", "bottle", 24, "pack", true),
	// Garnish and consumables (not sold on their own).
	counted("garnish_lemons", "Lemons", "garnish", "piece", 1, "piece", false),
	counted("garnish_mint", "Mint sprigs", "garnish", "sprig", 20, "bunch", false),
	counted("consumable_ice_bag", "Ice (bag)", "consumables", "bag", 1, "bag", false),
}

// CatalogByKey finds a catalogue entry.
func CatalogByKey(key string) (CatalogItem, bool) {
	for _, c := range Catalog {
		if c.Key == key {
			return c, true
		}
	}
	return CatalogItem{}, false
}
