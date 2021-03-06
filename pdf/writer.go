/*
 * This file is subject to the terms and conditions defined in
 * file 'LICENSE.txt', which is part of this source code package.
 */

// Default writing implementation.  Basic output with version 1.3
// for compatibility.

package pdf

import (
	"bufio"
	"crypto/md5"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/unidoc/unidoc/license"
)

type PdfWriter struct {
	root       *PdfIndirectObject
	pages      *PdfIndirectObject
	objects    []PdfObject
	objectsMap map[PdfObject]bool // Quick lookup table.
	writer     *bufio.Writer
	outlines   []*PdfIndirectObject
	catalog    *PdfObjectDictionary
	fields     []PdfObject
	infoObj    *PdfIndirectObject
	// Encryption
	crypter     *PdfCrypt
	encryptDict *PdfObjectDictionary
	encryptObj  *PdfIndirectObject
	ids         *PdfObjectArray
}

func NewPdfWriter() PdfWriter {
	w := PdfWriter{}

	w.objectsMap = map[PdfObject]bool{}
	w.objects = []PdfObject{}

	licenseKey := license.GetLicenseKey()

	producer := fmt.Sprintf("UniDoc Library version %s (%s) - http://unidoc.io", getUniDocVersion(), licenseKey.TypeToString())

	// Creation info.
	infoDict := PdfObjectDictionary{}
	infoDict[PdfObjectName("Producer")] = makeString(producer)
	infoDict[PdfObjectName("Creator")] = makeString("FoxyUtils Online PDF https://foxyutils.com")
	infoObj := PdfIndirectObject{}
	infoObj.PdfObject = &infoDict
	w.infoObj = &infoObj
	w.addObject(&infoObj)

	// Root catalog.
	catalog := PdfIndirectObject{}
	catalogDict := PdfObjectDictionary{}
	catalogDict[PdfObjectName("Type")] = makeName("Catalog")
	catalogDict[PdfObjectName("Version")] = makeName("1.3")
	catalog.PdfObject = &catalogDict

	w.root = &catalog
	w.addObject(&catalog)

	// Pages.
	pages := PdfIndirectObject{}
	pagedict := PdfObjectDictionary{}
	pagedict[PdfObjectName("Type")] = makeName("Pages")
	kids := PdfObjectArray{}
	pagedict[PdfObjectName("Kids")] = &kids
	pagedict[PdfObjectName("Count")] = makeInteger(0)
	pages.PdfObject = &pagedict

	w.pages = &pages
	w.addObject(&pages)

	catalogDict[PdfObjectName("Pages")] = &pages
	w.catalog = &catalogDict

	log.Info("Catalog %s", catalog)
	w.outlines = []*PdfIndirectObject{}

	return w
}

func (this *PdfWriter) hasObject(obj PdfObject) bool {
	// Check if already added.
	for _, o := range this.objects {
		// GH: May perform better to use a hash map to check if added?
		if o == obj {
			return true
		}
	}
	return false
}

// Adds the object to list of objects and returns true if the obj was
// not already added.
// Returns false if the object was previously added.
func (this *PdfWriter) addObject(obj PdfObject) bool {
	hasObj := this.hasObject(obj)
	if !hasObj {
		this.objects = append(this.objects, obj)
		return true
	}

	return false
}

func (this *PdfWriter) addObjects(obj PdfObject) error {
	log.Debug("Adding objects!")

	if io, isIndirectObj := obj.(*PdfIndirectObject); isIndirectObj {
		log.Debug("Indirect")
		log.Debug("- %s", obj)
		log.Debug("- %s", io.PdfObject)
		if this.addObject(io) {
			err := this.addObjects(io.PdfObject)
			if err != nil {
				return err
			}
		}
		return nil
	}

	if so, isStreamObj := obj.(*PdfObjectStream); isStreamObj {
		log.Debug("Stream")
		log.Debug("- %s", obj)
		if this.addObject(so) {
			err := this.addObjects(so.PdfObjectDictionary)
			if err != nil {
				return err
			}
		}
		return nil
	}

	if dict, isDict := obj.(*PdfObjectDictionary); isDict {
		log.Debug("Dict")
		log.Debug("- %s", obj)
		for k, v := range *dict {
			log.Debug("Key %s", k)
			if k != "Parent" {
				err := this.addObjects(v)
				if err != nil {
					return err
				}

			}
		}
		return nil
	}

	if arr, isArray := obj.(*PdfObjectArray); isArray {
		log.Debug("Array")
		log.Debug("- %s", obj)
		for _, v := range *arr {
			err := this.addObjects(v)
			if err != nil {
				return err
			}
		}
		return nil
	}

	if _, isReference := obj.(*PdfObjectReference); isReference {
		// Should never be a reference, should already be resolved.
		log.Error("Cannot be a reference!")
		return errors.New("Reference not allowed")
	}

	return nil
}

// Add a page to the PDF file. The new page should be an indirect
// object.
func (this *PdfWriter) AddPage(pageObj PdfObject) error {
	log.Debug("==========")
	log.Debug("Appending to page list")

	page, ok := pageObj.(*PdfIndirectObject)
	if !ok {
		return errors.New("Page should be an indirect object")
	}
	log.Debug("%s", page)
	log.Debug("%s", page.PdfObject)

	pDict := page.PdfObject.(*PdfObjectDictionary)
	otype := (*pDict)["Type"].(*PdfObjectName)
	if *otype != "Page" {
		return errors.New("Type != Page (Required).")
	}

	// Copy inherited fields if missing.
	inheritedFields := []PdfObjectName{"Resources", "MediaBox", "CropBox", "Rotate"}
	parent, hasParent := (*pDict)["Parent"].(*PdfIndirectObject)
	log.Debug("Page Parent: %T (%v)", (*pDict)["Parent"], hasParent)
	for hasParent {
		log.Debug("Page Parent: %T", parent)
		parentDict, ok := parent.PdfObject.(*PdfObjectDictionary)
		if !ok {
			return errors.New("Invalid Parent object")
		}
		for _, field := range inheritedFields {
			log.Debug("Field %s", field)
			if _, hasAlready := (*pDict)[field]; hasAlready {
				log.Debug("- page has already")
				continue
			}

			if obj, hasField := (*parentDict)[field]; hasField {
				// Parent has the field.  Inherit, pass to the new page.
				log.Debug("Inheriting field %s", field)
				(*pDict)[field] = obj
			}
		}
		parent, hasParent = (*parentDict)["Parent"].(*PdfIndirectObject)
		log.Debug("Next parent: %T", (*parentDict)["Parent"])
	}

	log.Debug("Traversal done")

	// Update the dictionary.
	// Reuses the input object, updating the fields.
	(*pDict)["Parent"] = this.pages
	page.PdfObject = pDict

	// Add to Pages.
	pagesDict := this.pages.PdfObject.(*PdfObjectDictionary)
	kids := (*pagesDict)["Kids"].(*PdfObjectArray)
	*kids = append(*kids, page)
	pageCount := (*pagesDict)["Count"].(*PdfObjectInteger)
	*pageCount = *pageCount + 1

	this.addObject(page)

	// Traverse the page and record all object references.
	err := this.addObjects(pDict)
	if err != nil {
		return err
	}

	return nil
}

// Add outlines to a PDF file.
func (this *PdfWriter) AddOutlines(outlinesList []*PdfIndirectObject) error {
	// Add the outlines.
	for _, outline := range outlinesList {
		this.outlines = append(this.outlines, outline)
	}
	return nil
}

// Look for a specific key.  Returns a list of entries.
// What if something appears on many pages?
func (this *PdfWriter) seekByName(obj PdfObject, followKeys []string, key string) ([]PdfObject, error) {
	log.Debug("Seek by name.. %T", obj)
	list := []PdfObject{}
	if io, isIndirectObj := obj.(*PdfIndirectObject); isIndirectObj {
		return this.seekByName(io.PdfObject, followKeys, key)
	}

	if so, isStreamObj := obj.(*PdfObjectStream); isStreamObj {
		return this.seekByName(so.PdfObjectDictionary, followKeys, key)
	}

	if dict, isDict := obj.(*PdfObjectDictionary); isDict {
		log.Debug("Dict")
		for k, v := range *dict {
			if string(k) == key {
				list = append(list, v)
			}
			for _, followKey := range followKeys {
				if string(k) == followKey {
					log.Debug("Follow key %s", followKey)
					items, err := this.seekByName(v, followKeys, key)
					if err != nil {
						return list, err
					}
					for _, item := range items {
						list = append(list, item)
					}
					break
				}
			}
		}
		return list, nil
	}

	// Ignore arrays.
	//if arr, isArray := obj.(*PdfObjectArray); isArray {
	//}

	return list, nil
}

// Add Acroforms to a PDF file.
func (this *PdfWriter) AddForms(forms *PdfObjectDictionary) error {
	// Traverse the forms object...
	// Keep a list of stuff?

	// Forms dictionary should have:
	// Fields array.
	if forms == nil {
		return errors.New("forms == nil")
	}

	// For now, support only regular forms with fields
	var fieldsArray *PdfObjectArray
	if fields, hasFields := (*forms)["Fields"]; hasFields {
		if arr, isArray := fields.(*PdfObjectArray); isArray {
			fieldsArray = arr
		} else if ind, isInd := fields.(*PdfIndirectObject); isInd {
			if arr, isArray := ind.PdfObject.(*PdfObjectArray); isArray {
				fieldsArray = arr
			}
		}
	}
	if fieldsArray == nil {
		log.Debug("Writer - no fields to be added to forms")
		return nil
	}

	// Add the fields.
	for _, field := range *fieldsArray {
		fieldObj, ok := field.(*PdfIndirectObject)
		if !ok {
			return errors.New("Field not pointing indirect object")
		}

		followKeys := []string{"Fields", "Kids"}
		list, err := this.seekByName(fieldObj, followKeys, "P")
		log.Debug("Done seeking!")
		if err != nil {
			return err
		}
		log.Debug("List of P objects %d", len(list))
		if len(list) < 1 {
			continue
		}

		includeField := false
		for _, p := range list {
			if po, ok := p.(*PdfIndirectObject); ok {
				log.Debug("P entry is an indirect object (page)")
				if this.hasObject(po) {
					includeField = true
				} else {
					return errors.New("P pointing outside of write pages")
				}
			} else {
				log.Error("P entry not an indirect object (%T)", p)
			}
		}

		// This won't work.  There can be many sub objects.
		// Need to specifically go and check the page object!
		// P or the appearance dictionary.
		if includeField {
			log.Debug("Add the field! (%T)", field)
			// Add if nothing referenced outside of the writer.
			// Probably need to add some objects first...
			this.addObject(field)
			this.fields = append(this.fields, field)
		} else {
			log.Debug("Field not relevant!")
		}
	}
	return nil
}

// Write out an indirect / stream object.
func (this *PdfWriter) writeObject(num int, obj PdfObject) {
	log.Debug("Write obj #%d\n", num)

	if pobj, isIndirect := obj.(*PdfIndirectObject); isIndirect {
		outStr := fmt.Sprintf("%d 0 obj\n", num)
		outStr += pobj.PdfObject.DefaultWriteString()
		outStr += "\nendobj\n"
		this.writer.WriteString(outStr)
		return
	}

	if pobj, isStream := obj.(*PdfObjectStream); isStream {
		outStr := fmt.Sprintf("%d 0 obj\n", num)
		outStr += pobj.PdfObjectDictionary.DefaultWriteString()
		outStr += "\nstream\n"
		this.writer.WriteString(outStr)
		this.writer.Write(pobj.Stream)
		this.writer.WriteString("\nendstream\nendobj\n")
		return
	}

	this.writer.WriteString(obj.DefaultWriteString())
}

// Update all the object numbers prior to writing.
func (this *PdfWriter) updateObjectNumbers() {
	// Update numbers
	for idx, obj := range this.objects {
		if io, isIndirect := obj.(*PdfIndirectObject); isIndirect {
			io.ObjectNumber = int64(idx + 1)
			io.GenerationNumber = 0
		}
		if so, isStream := obj.(*PdfObjectStream); isStream {
			so.ObjectNumber = int64(idx + 1)
			so.GenerationNumber = 0
		}
	}
}

type EncryptOptions struct {
	Permissions AccessPermissions
}

// Encrypt the output file with a specified user/owner password.
func (this *PdfWriter) Encrypt(userPass, ownerPass []byte, options *EncryptOptions) error {
	crypter := PdfCrypt{}
	this.crypter = &crypter

	crypter.encryptedObjects = map[PdfObject]bool{}

	crypter.cryptFilters = CryptFilters{}
	crypter.cryptFilters["Default"] = CryptFilter{cfm: "V2", length: 128}

	// Set
	crypter.P = -1
	crypter.V = 2
	crypter.R = 3
	crypter.length = 128
	crypter.encryptMetadata = true
	if options != nil {
		crypter.P = int(options.Permissions.GetP())
	}

	// Prepare the ID object for the trailer.
	hashcode := md5.Sum([]byte(time.Now().Format(time.RFC850)))
	id0 := PdfObjectString(hashcode[:])
	b := make([]byte, 100)
	rand.Read(b)
	hashcode = md5.Sum(b)
	id1 := PdfObjectString(hashcode[:])
	log.Debug("Random b: % x", b)

	this.ids = &PdfObjectArray{&id0, &id1}
	log.Debug("Gen Id 0: % x", id0)

	crypter.id0 = string(id0)

	// Make the O and U objects.
	O, err := crypter.alg3(userPass, ownerPass)
	if err != nil {
		log.Error("Error generating O for encryption (%s)", err)
		return err
	}
	crypter.O = []byte(O)
	log.Debug("gen O: % x", O)
	U, key, err := crypter.alg5(userPass)
	if err != nil {
		log.Error("Error generating O for encryption (%s)", err)
		return err
	}
	log.Debug("gen U: % x", U)
	crypter.U = []byte(U)
	crypter.encryptionKey = key

	// Generate the encryption dictionary.
	encDict := &PdfObjectDictionary{}
	(*encDict)[PdfObjectName("Filter")] = makeName("Standard")
	(*encDict)[PdfObjectName("P")] = makeInteger(int64(crypter.P))
	(*encDict)[PdfObjectName("V")] = makeInteger(int64(crypter.V))
	(*encDict)[PdfObjectName("R")] = makeInteger(int64(crypter.R))
	(*encDict)[PdfObjectName("Length")] = makeInteger(int64(crypter.length))
	(*encDict)[PdfObjectName("O")] = &O
	(*encDict)[PdfObjectName("U")] = &U
	this.encryptDict = encDict

	// Make an object to contain it.
	io := &PdfIndirectObject{}
	io.PdfObject = encDict
	this.encryptObj = io
	this.addObject(io)

	return nil
}

// Write the pdf out.
func (this *PdfWriter) Write(ws io.WriteSeeker) error {
	log.Debug("Write()")
	if len(this.outlines) > 0 {
		// Add the outlines dictionary if some outlines added.
		// Assume they are correct, not referencing anything not added
		// for writing.
		outlines := PdfIndirectObject{}
		outlinesDict := PdfObjectDictionary{}
		outlinesDict[PdfObjectName("Type")] = makeName("Outlines")
		outlinesDict[PdfObjectName("First")] = this.outlines[0]
		outlinesDict[PdfObjectName("Last")] = this.outlines[len(this.outlines)-1]
		outlines.PdfObject = &outlinesDict
		(*this.catalog)[PdfObjectName("Outlines")] = &outlines

		for idx, outline := range this.outlines {
			dict, ok := outline.PdfObject.(*PdfObjectDictionary)
			if !ok {
				continue
			}
			if idx < len(this.outlines)-1 {
				(*dict)[PdfObjectName("Next")] = this.outlines[idx+1]
			}
			if idx > 0 {
				(*dict)[PdfObjectName("Prev")] = this.outlines[idx-1]
			}
			(*dict)[PdfObjectName("Parent")] = &outlines
		}
		err := this.addObjects(&outlines)
		if err != nil {
			return err
		}
	}
	if len(this.fields) > 0 {
		forms := PdfIndirectObject{}
		formsDict := PdfObjectDictionary{}
		forms.PdfObject = &formsDict
		fieldsArray := PdfObjectArray{}
		for _, field := range this.fields {
			fieldsArray = append(fieldsArray, field)
		}
		formsDict[PdfObjectName("Fields")] = &fieldsArray
		(*this.catalog)[PdfObjectName("AcroForm")] = &forms
		err := this.addObjects(&forms)
		if err != nil {
			return err
		}
	}

	w := bufio.NewWriter(ws)
	this.writer = w

	w.WriteString("%PDF-1.3\n")
	w.WriteString("%âãÏÓ\n")
	w.Flush()

	this.updateObjectNumbers()

	offsets := []int64{}

	// Write objects
	log.Debug("Writing %d obj", len(this.objects))
	for idx, obj := range this.objects {
		log.Debug("Writing %d", idx)
		this.writer.Flush()
		offset, _ := ws.Seek(0, os.SEEK_CUR)
		offsets = append(offsets, offset)

		// Encrypt prior to writing.
		// Encrypt dictionary should not be encrypted.
		if this.crypter != nil && obj != this.encryptObj {
			err := this.crypter.Encrypt(obj, int64(idx+1), 0)
			if err != nil {
				log.Error("Failed encrypting (%s)", err)
				return err
			}

		}
		this.writeObject(idx+1, obj)
	}
	w.Flush()

	xrefOffset, _ := ws.Seek(0, os.SEEK_CUR)
	// Write xref table.
	this.writer.WriteString("xref\r\n")
	outStr := fmt.Sprintf("%d %d\r\n", 0, len(this.objects)+1)
	this.writer.WriteString(outStr)
	outStr = fmt.Sprintf("%.10d %.5d f\r\n", 0, 65535)
	this.writer.WriteString(outStr)
	for _, offset := range offsets {
		outStr = fmt.Sprintf("%.10d %.5d n\r\n", offset, 0)
		this.writer.WriteString(outStr)
	}

	// Generate & write trailer
	trailer := PdfObjectDictionary{}
	trailer["Info"] = this.infoObj
	trailer["Root"] = this.root
	trailer["Size"] = makeInteger(int64(len(this.objects) + 1))
	// If encrypted!
	if this.crypter != nil {
		trailer["Encrypt"] = this.encryptObj
		trailer[PdfObjectName("ID")] = this.ids
		log.Debug("Ids: %s", this.ids)
	}
	this.writer.WriteString("trailer\n")
	this.writer.WriteString(trailer.DefaultWriteString())
	this.writer.WriteString("\n")

	// Make offset reference.
	outStr = fmt.Sprintf("startxref\n%d\n", xrefOffset)
	this.writer.WriteString(outStr)
	this.writer.WriteString("%%EOF\n")
	w.Flush()

	return nil
}
