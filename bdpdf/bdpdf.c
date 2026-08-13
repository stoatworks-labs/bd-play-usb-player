// bdpdf — rasterise PDF pages to PNG for the BirdDog PLAY.
//
// The PLAY's rootfs has no PDF renderer of any kind. bdplay shells out to this
// to turn a document on a USB stick into page images it can show as stills.
//
// WHY PDFIUM AND NOT MUPDF
// ------------------------
// MuPDF is AGPL v3, which makes it impossible to ship from the public web
// patcher: serving it to arbitrary users is distribution, and §13 reaches
// network-interactive use, which a web-driven signage player plainly is. PDFium
// is BSD-3-Clause, ~8 MB rather than 37 MB, and its prebuilt linux-arm64 build
// needs only GLIBC_2.17 (the device has 2.28) and no libstdc++ — it links its
// own C++ runtime statically. So this is hostable, and PDF becomes an ordinary
// checkbox rather than a licence problem.
//
// bdplay keeps its mutool backend too, for anyone who already has one.
//
// Output is PNG via stb_image_write (public domain), so there is no libpng
// dependency to satisfy on a device that has no development packages.

#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

#include "fpdfview.h"

#define STB_IMAGE_WRITE_IMPLEMENTATION
// Do NOT define STBI_WRITE_NO_STDIO here, not even to 0: stb tests it with
// #ifndef, so defining it at all removes stbi_write_png() and the build fails
// on an implicit declaration.
#include "stb_image_write.h"

#define DEFAULT_MAX_PAGES 200

static void usage(void) {
    fputs(
        "bdpdf — render PDF pages to PNG\n"
        "\n"
        "  bdpdf -o DIR -w WIDTH -h HEIGHT [-n MAXPAGES] [-p PASSWORD] FILE.pdf\n"
        "  bdpdf -info FILE.pdf\n"
        "\n"
        "Pages are written as DIR/page-0001.png, scaled to fit WIDTH x HEIGHT\n"
        "with aspect ratio preserved. -info prints the page count and exits.\n",
        stderr);
}

// Map PDFium's error enum onto something a user can act on. The generic
// \"failed to load\" message is useless when the real cause is a password.
static const char *pdf_error(void) {
    switch (FPDF_GetLastError()) {
        case FPDF_ERR_SUCCESS:  return "no error";
        case FPDF_ERR_FILE:     return "file not found or unreadable";
        case FPDF_ERR_FORMAT:   return "not a PDF, or the file is corrupt";
        case FPDF_ERR_PASSWORD: return "password required or incorrect";
        case FPDF_ERR_SECURITY: return "unsupported security scheme";
        case FPDF_ERR_PAGE:     return "page not found or damaged";
        default:                return "unknown error";
    }
}

int main(int argc, char **argv) {
    const char *outdir = NULL, *input = NULL, *password = NULL;
    int want_w = 1920, want_h = 1080, max_pages = DEFAULT_MAX_PAGES, info_only = 0;

    for (int i = 1; i < argc; i++) {
        if (!strcmp(argv[i], "-info")) { info_only = 1; }
        else if (!strcmp(argv[i], "-o") && i + 1 < argc) { outdir = argv[++i]; }
        else if (!strcmp(argv[i], "-w") && i + 1 < argc) { want_w = atoi(argv[++i]); }
        else if (!strcmp(argv[i], "-h") && i + 1 < argc) { want_h = atoi(argv[++i]); }
        else if (!strcmp(argv[i], "-n") && i + 1 < argc) { max_pages = atoi(argv[++i]); }
        else if (!strcmp(argv[i], "-p") && i + 1 < argc) { password = argv[++i]; }
        else if (argv[i][0] == '-') { usage(); return 2; }
        else { input = argv[i]; }
    }

    if (!input || (!info_only && !outdir)) { usage(); return 2; }
    if (want_w < 1 || want_h < 1 || want_w > 16384 || want_h > 16384) {
        fprintf(stderr, "bdpdf: implausible output size %dx%d\n", want_w, want_h);
        return 2;
    }

    FPDF_InitLibrary();

    FPDF_DOCUMENT doc = FPDF_LoadDocument(input, password);
    if (!doc) {
        fprintf(stderr, "bdpdf: %s: %s\n", input, pdf_error());
        FPDF_DestroyLibrary();
        return 1;
    }

    int pages = FPDF_GetPageCount(doc);
    if (info_only) {
        printf("Pages: %d\n", pages);
        FPDF_CloseDocument(doc);
        FPDF_DestroyLibrary();
        return 0;
    }

    // Cap the work. A 500-page manual on a signage loop is a mistake, and
    // rendering it would fill /userdata.
    if (max_pages > 0 && pages > max_pages) pages = max_pages;

    int written = 0, failed = 0;
    for (int i = 0; i < pages; i++) {
        FPDF_PAGE page = FPDF_LoadPage(doc, i);
        if (!page) {
            fprintf(stderr, "bdpdf: page %d: %s\n", i + 1, pdf_error());
            failed++;
            continue;
        }

        double pw = FPDF_GetPageWidth(page);
        double ph = FPDF_GetPageHeight(page);
        if (pw <= 0 || ph <= 0) {
            fprintf(stderr, "bdpdf: page %d has zero size\n", i + 1);
            FPDF_ClosePage(page);
            failed++;
            continue;
        }

        // Fit inside the panel, preserving aspect. bdplay letterboxes the rest
        // with videoscale add-borders, so we must not stretch here.
        double scale = (double)want_w / pw;
        double sh = (double)want_h / ph;
        if (sh < scale) scale = sh;

        int w = (int)(pw * scale + 0.5);
        int h = (int)(ph * scale + 0.5);
        if (w < 1) w = 1;
        if (h < 1) h = 1;

        FPDF_BITMAP bmp = FPDFBitmap_Create(w, h, 0 /* no alpha channel */);
        if (!bmp) {
            fprintf(stderr, "bdpdf: page %d: out of memory at %dx%d\n", i + 1, w, h);
            FPDF_ClosePage(page);
            failed++;
            continue;
        }

        // White background. Without this, pages with no explicit background
        // render onto uninitialised memory and come out as noise.
        FPDFBitmap_FillRect(bmp, 0, 0, w, h, 0xFFFFFFFF);
        FPDF_RenderPageBitmap(bmp, page, 0, 0, w, h, 0, FPDF_ANNOT);

        // PDFium hands back BGRA (it ignores the alpha flag for the buffer
        // layout and always uses 4 bytes here); stb wants RGB. Repack rather
        // than writing RGBA, so the cached pages stay a third smaller.
        uint8_t *src = (uint8_t *)FPDFBitmap_GetBuffer(bmp);
        int stride = FPDFBitmap_GetStride(bmp);
        uint8_t *rgb = (uint8_t *)malloc((size_t)w * h * 3);
        if (!rgb) {
            fprintf(stderr, "bdpdf: page %d: out of memory packing %dx%d\n", i + 1, w, h);
            FPDFBitmap_Destroy(bmp);
            FPDF_ClosePage(page);
            failed++;
            continue;
        }
        for (int y = 0; y < h; y++) {
            const uint8_t *r = src + (size_t)y * stride;
            uint8_t *o = rgb + (size_t)y * w * 3;
            for (int x = 0; x < w; x++) {
                o[x * 3 + 0] = r[x * 4 + 2];  // R
                o[x * 3 + 1] = r[x * 4 + 1];  // G
                o[x * 3 + 2] = r[x * 4 + 0];  // B
            }
        }

        char path[4096];
        snprintf(path, sizeof(path), "%s/page-%04d.png", outdir, i + 1);
        if (!stbi_write_png(path, w, h, 3, rgb, w * 3)) {
            fprintf(stderr, "bdpdf: page %d: could not write %s\n", i + 1, path);
            failed++;
        } else {
            written++;
        }

        free(rgb);
        FPDFBitmap_Destroy(bmp);
        FPDF_ClosePage(page);
    }

    FPDF_CloseDocument(doc);
    FPDF_DestroyLibrary();

    if (written == 0) {
        fprintf(stderr, "bdpdf: no pages rendered\n");
        return 1;
    }
    // A partially rendered document still plays; say so but succeed, so one
    // damaged page does not cost the whole deck.
    if (failed) fprintf(stderr, "bdpdf: %d page(s) failed, %d written\n", failed, written);
    printf("%d\n", written);
    return 0;
}
