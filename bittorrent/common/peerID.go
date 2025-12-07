package common

import (
	"fmt"
	"strings"
)

type PeerID string

func (self *PeerID) Valid() bool {
	if len(*self) == 20 {
		return true
	}
	return false
}

// DecodeClient decodes the client software name and version from peer_id
// Supports Azureus-style encoding (most common) and other formats
func (self *PeerID) DecodeClient() string {
	peerID := string(*self)
	if len(peerID) < 8 {
		return "unknown"
	}

	// Azureus-style encoding: -XX####-xxxxxxxxxxxx
	// Examples: -qB4610-, -TR300Z-, -LT2210-, -UT3530-
	if peerID[0] == '-' && len(peerID) >= 8 {
		clientID := peerID[1:3]
		versionStr := peerID[3:7]

		// Map client IDs to names
		clientNames := map[string]string{
			"qB": "qBittorrent",
			"TR": "Transmission",
			"LT": "libtorrent",
			"UT": "uTorrent",
			"BT": "BitTorrent",
			"DE": "Deluge",
			"AZ": "Azureus/Vuze",
			"BC": "BitComet",
			"BS": "BTSlave",
			"CD": "Enhanced CTorrent",
			"KT": "KTorrent",
			"LH": "LH-ABC",
			"MP": "MooPolice",
			"MT": "MoonlightTorrent",
			"NX": "Net Transport",
			"PD": "Pando",
			"QT": "Qt 4 Torrent",
			"RT": "Retriever",
			"SZ": "Shareaza",
			"SB": "Swiftbit",
			"SS": "SwarmScope",
			"TN": "TorrentDotNET",
			"TS": "Torrentstorm",
			"XT": "XanTorrent",
			"ZT": "ZipTorrent",
			"AR": "Arctic",
			"AV": "Avicora",
			"AX": "BitPump",
			"BB": "BitBuddy",
			"BF": "Bitflu",
			"BG": "BTG",
			"BR": "BitRocket",
			"BX": "BittorrentX",
			"CT": "CTorrent",
			"DP": "Propagate Data Client",
			"EB": "EBit",
			"ES": "electric sheep",
			"FT": "FoxTorrent",
			"FW": "FrostWire",
			"FX": "Freebox BitTorrent",
			"GS": "GSTorrent",
			"HK": "Hekate",
			"HL": "Halite",
			"HM": "hMule",
			"HN": "Hydranode",
			"IL": "iLivid",
			"JS": "Justseed.it client",
			"KG": "KGet",
			"LC": "LeechCraft",
			"LP": "Lphant",
			"MO": "MonoTorrent",
			"MR": "Miro",
			"NB": "Net::BitTorrent",
			"OS": "OneSwarm",
			"OT": "OmegaTorrent",
			"PB": "Protocol::BitTorrent",
			"PI": "PicoTorrent",
			"QD": "QQDownload",
			"SD": "Thunder",
			"SM": "SoMud",
			"SP": "BitSpirit",
			"ST": "SymTorrent",
			"SX": "Seedbox",
			"TL": "Tribler",
			"TT": "TuoTu",
			"UL": "uLeecher!",
			"UM": "µTorrent Mac",
			"VG": "Vagaa",
			"WT": "BitLet",
			"WW": "WebTorrent",
			"WY": "FireTorrent",
			"XL": "Xunlei",
			"XS": "XSwifter",
			"XX": "Xtorrent",
		}

		clientName, ok := clientNames[clientID]
		if !ok {
			// Unknown client ID, return the ID itself
			clientName = clientID
		}

		// Decode version: #### -> X.X.X format
		// Example: 4610 -> 4.6.10
		version := decodeVersion(versionStr)

		if version != "" {
			return fmt.Sprintf("%s %s", clientName, version)
		}
		return clientName
	}

	// Shadow-style encoding (older format)
	// Check single character first (Mainline)
	if len(peerID) >= 1 && peerID[0] == 'M' {
		return "Mainline"
	}

	// Then check 2-character prefixes
	if len(peerID) >= 2 {
		prefix := peerID[0:2]
		shadowClients := map[string]string{
			"ex": "BitComet",
			"FC": "FileCroc",
			"FD": "Free Download Manager",
		}

		if name, ok := shadowClients[prefix]; ok {
			return name
		}
	}

	// BitComet style: starts with "exbc"
	if strings.HasPrefix(peerID, "exbc") {
		return "BitComet"
	}

	return "unknown"
}

// decodeVersion converts version string like "4610" to "4.6.10"
// Also handles formats like "300Z" (Transmission) -> "3.0.0"
func decodeVersion(versionStr string) string {
	if len(versionStr) != 4 {
		return ""
	}

	// Check if all characters are digits
	allDigits := true
	for _, c := range versionStr {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}

	if allDigits {
		// Try to parse as X.X.XX format (e.g., "4610" -> "4.6.10")
		var major, minor, patch int
		if _, err := fmt.Sscanf(versionStr, "%1d%1d%2d", &major, &minor, &patch); err == nil {
			return fmt.Sprintf("%d.%d.%d", major, minor, patch)
		}

		// If that fails, try as XX.XX format (e.g., "1234" -> "12.34")
		if _, err := fmt.Sscanf(versionStr, "%2d%2d", &major, &minor); err == nil {
			return fmt.Sprintf("%d.%d", major, minor)
		}
	} else {
		// Handle formats with letters (e.g., "300Z" -> "3.0.0")
		// Extract numeric parts
		var major, minor, patch int
		if n, _ := fmt.Sscanf(versionStr, "%1d%1d%1d", &major, &minor, &patch); n == 3 {
			return fmt.Sprintf("%d.%d.%d", major, minor, patch)
		}
		// Try 2+1+1 format
		if n, _ := fmt.Sscanf(versionStr, "%2d%1d%1d", &major, &minor, &patch); n == 3 {
			return fmt.Sprintf("%d.%d.%d", major, minor, patch)
		}
		// Try 1+2+1 format
		if n, _ := fmt.Sscanf(versionStr, "%1d%2d%1d", &major, &minor, &patch); n == 3 {
			return fmt.Sprintf("%d.%d.%d", major, minor, patch)
		}
		// Try 2+2 format with letters
		if n, _ := fmt.Sscanf(versionStr, "%2d%2d", &major, &minor); n == 2 {
			return fmt.Sprintf("%d.%d", major, minor)
		}
	}

	// If all parsing fails, return the version string as-is (may contain letters)
	return versionStr
}
