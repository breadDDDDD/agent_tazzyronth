// Package assets embeds the pre-rendered logo art.
//
// The .ans/.txt files are generated from logo.jfif and committed, so normal
// builds don't need to run the converter.
package assets

//go:generate go run ../tools/logo2ansi -in logo.jfif -out .

import _ "embed"

var (
	// LogoFull is the ~40-column truecolor splash.
	//go:embed logo_full.ans
	LogoFull string

	// LogoFullPlain is LogoFull without colour, for NO_COLOR or limited terminals.
	//go:embed logo_full.txt
	LogoFullPlain string

	// LogoMini is the 3-row truecolor mark for headers.
	//go:embed logo_mini.ans
	LogoMini string

	// LogoMiniPlain is LogoMini without colour.
	//go:embed logo_mini.txt
	LogoMiniPlain string

	// LogoPNG is a small transparent PNG of the logo for the web view.
	//go:embed logo_web.png
	LogoPNG []byte
)
