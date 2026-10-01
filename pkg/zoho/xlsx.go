package zoho

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
)

// readXLSX returns the rows of the first worksheet of an .xlsx file
// (Office Open XML: a zip of XML parts; shared strings and inline strings).
func readXLSX(data []byte) ([][]string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, errors.New("this is not a valid .xlsx file")
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[strings.TrimPrefix(f.Name, "/")] = f
	}
	read := func(name string) ([]byte, error) {
		f := files[name]
		if f == nil {
			return nil, errors.New("missing " + name)
		}
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		return io.ReadAll(io.LimitReader(rc, 200<<20))
	}

	// First sheet: workbook.xml → relationship → target.
	sheetPath := "xl/worksheets/sheet1.xml"
	if wb, err := read("xl/workbook.xml"); err == nil {
		var w struct {
			Sheets []struct {
				RID string `xml:"http://schemas.openxmlformats.org/officeDocument/2006/relationships id,attr"`
			} `xml:"sheets>sheet"`
		}
		if xml.Unmarshal(wb, &w) == nil && len(w.Sheets) > 0 {
			if rels, err := read("xl/_rels/workbook.xml.rels"); err == nil {
				var r struct {
					Rels []struct {
						ID     string `xml:"Id,attr"`
						Target string `xml:"Target,attr"`
					} `xml:"Relationship"`
				}
				if xml.Unmarshal(rels, &r) == nil {
					for _, rel := range r.Rels {
						if rel.ID == w.Sheets[0].RID {
							t := rel.Target
							if strings.HasPrefix(t, "/") {
								sheetPath = strings.TrimPrefix(t, "/")
							} else {
								sheetPath = path.Clean("xl/" + t)
							}
						}
					}
				}
			}
		}
	}

	var shared []string
	if ss, err := read("xl/sharedStrings.xml"); err == nil {
		var sst struct {
			SI []struct {
				T string `xml:"t"`
				R []struct {
					T string `xml:"t"`
				} `xml:"r"`
			} `xml:"si"`
		}
		if err := xml.Unmarshal(ss, &sst); err != nil {
			return nil, errors.New("could not read the shared strings of the .xlsx file")
		}
		for _, si := range sst.SI {
			s := si.T
			for _, r := range si.R {
				s += r.T
			}
			shared = append(shared, s)
		}
	}

	sheet, err := read(sheetPath)
	if err != nil {
		return nil, errors.New("the .xlsx file has no worksheet")
	}
	var ws struct {
		Rows []struct {
			R     int `xml:"r,attr"`
			Cells []struct {
				Ref string `xml:"r,attr"`
				T   string `xml:"t,attr"`
				V   string `xml:"v"`
				Is  struct {
					T string `xml:"t"`
					R []struct {
						T string `xml:"t"`
					} `xml:"r"`
				} `xml:"is"`
			} `xml:"c"`
		} `xml:"sheetData>row"`
	}
	if err := xml.Unmarshal(sheet, &ws); err != nil {
		return nil, errors.New("could not read the worksheet of the .xlsx file")
	}
	var out [][]string
	for _, row := range ws.Rows {
		var cells []string
		next := 0
		for _, c := range row.Cells {
			col := next
			if c.Ref != "" {
				col = colIndex(c.Ref)
			}
			for len(cells) < col {
				cells = append(cells, "")
			}
			v := c.V
			switch c.T {
			case "s":
				if i, err := strconv.Atoi(strings.TrimSpace(c.V)); err == nil && i >= 0 && i < len(shared) {
					v = shared[i]
				}
			case "inlineStr":
				v = c.Is.T
				for _, r := range c.Is.R {
					v += r.T
				}
			case "b":
				if v == "1" {
					v = "true"
				} else {
					v = "false"
				}
			}
			cells = append(cells, v)
			next = col + 1
		}
		out = append(out, cells)
	}
	return out, nil
}

// colIndex turns "C12" into 2.
func colIndex(ref string) int {
	n := 0
	for _, r := range ref {
		if r < 'A' || r > 'Z' {
			break
		}
		n = n*26 + int(r-'A'+1)
	}
	return n - 1
}
