/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (c) 2026 CEMAXECUTER LLC
 *
 * HackRF native backend for inmarsat-sniffer.
 * Ported from iridium-sniffer's hackrf.c.
 *
 */

#include <err.h>
#include <pthread.h>
#include <signal.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>

#include <libhackrf/hackrf.h>

#include "sdr.h"
#include "hackrf.h"

extern double samp_rate;
extern double center_freq;
extern int bias_tee;
extern volatile sig_atomic_t running;

extern int hackrf_lna_gain;
extern int hackrf_vga_gain;
extern int hackrf_amp_enable;

extern void push_samples(sample_buf_t *buf);

#define HACKRF_STALL_SEC       3
#define HACKRF_RECONNECT_SEC   2
#define HACKRF_POLL_MS         500

/* Active device while hackrf_start_rx() is running; used to call
 * hackrf_stop_rx() from the main thread on SIGINT so a libusb callback
 * blocked in push_samples() can return. */
static _Atomic(hackrf_device *) g_hackrf_active;

static volatile time_t hackrf_last_rx;
static volatile int    hackrf_rx_active;

struct hackrf_stream_ctx {
    char *serial; /* strdup'd, NULL = first device */
};

static int hackrf_device_present(const char *serial)
{
    hackrf_device_list_t *list = hackrf_device_list();
    if (!list || list->devicecount == 0) {
        if (list) hackrf_device_list_free(list);
        return 0;
    }
    if (!serial || !serial[0]) {
        hackrf_device_list_free(list);
        return 1;
    }
    int found = 0;
    for (int i = 0; i < list->devicecount; i++) {
        const char *s = list->serial_numbers[i];
        if (!s) continue;
        if (strcmp(s, serial) == 0) {
            found = 1;
            break;
        }
        /* --hackrf= suffix may omit leading zeros */
        while (*s == '0') s++;
        const char *want = serial;
        while (*want == '0') want++;
        if (strcmp(s, want) == 0) {
            found = 1;
            break;
        }
    }
    hackrf_device_list_free(list);
    return found;
}

static int hackrf_configure(hackrf_device *dev)
{
    int r;

    r = hackrf_set_sample_rate(dev, samp_rate);
    if (r != HACKRF_SUCCESS)
        return r;

    r = hackrf_set_freq(dev, (uint64_t)center_freq);
    if (r != HACKRF_SUCCESS)
        return r;

    r = hackrf_set_lna_gain(dev, hackrf_lna_gain);
    if (r != HACKRF_SUCCESS)
        warnx("HackRF set_lna_gain(%d): %s", hackrf_lna_gain, hackrf_error_name(r));

    r = hackrf_set_vga_gain(dev, hackrf_vga_gain);
    if (r != HACKRF_SUCCESS)
        warnx("HackRF set_vga_gain(%d): %s", hackrf_vga_gain, hackrf_error_name(r));

    if (hackrf_amp_enable) {
        r = hackrf_set_amp_enable(dev, 1);
        if (r != HACKRF_SUCCESS)
            warnx("HackRF set_amp_enable: %s", hackrf_error_name(r));
    }

    if (bias_tee) {
        r = hackrf_set_antenna_enable(dev, 1);
        if (r != HACKRF_SUCCESS)
            warnx("HackRF bias-tee enable: %s", hackrf_error_name(r));
    }

    return HACKRF_SUCCESS;
}

static hackrf_device *hackrf_open_configured(const char *serial)
{
    hackrf_device *dev = NULL;
    int r;

    if (serial && serial[0])
        r = hackrf_open_by_serial(serial, &dev);
    else
        r = hackrf_open(&dev);
    if (r != HACKRF_SUCCESS)
        return NULL;

    r = hackrf_configure(dev);
    if (r != HACKRF_SUCCESS) {
        warnx("HackRF configure failed: %s", hackrf_error_name(r));
        hackrf_close(dev);
        return NULL;
    }

    fprintf(stderr, "HackRF: serial=%s sr=%.0f freq=%.0f lna=%d vga=%d amp=%d\n",
            serial ? serial : "(auto)", samp_rate, center_freq,
            hackrf_lna_gain, hackrf_vga_gain, hackrf_amp_enable);
    return dev;
}

static void hackrf_close_session(hackrf_device *dev)
{
    if (!dev) return;
    atomic_store(&g_hackrf_active, NULL);
    hackrf_stop_rx(dev);
    hackrf_close(dev);
}

void hackrf_backend_list(void)
{
    hackrf_init();
    hackrf_device_list_t *list = hackrf_device_list();
    if (!list || list->devicecount == 0) {
        printf("  (no HackRF devices found)\n");
        if (list) hackrf_device_list_free(list);
        return;
    }
    for (int i = 0; i < list->devicecount; i++) {
        const char *s = list->serial_numbers[i];
        /* trim leading zeros from serial for cleaner display */
        while (*s == '0') s++;
        printf("  hackrf-%-24s HackRF One\n", s);
    }
    hackrf_device_list_free(list);
}

hackrf_stream_ctx_t *hackrf_stream_ctx_create(const char *serial)
{
    hackrf_stream_ctx_t *ctx = calloc(1, sizeof(*ctx));
    if (!ctx)
        errx(1, "HackRF: out of memory");
    if (serial && serial[0])
        ctx->serial = strdup(serial);
    return ctx;
}

void *hackrf_backend_setup(const char *serial)
{
    return hackrf_stream_ctx_create(serial);
}

static int hackrf_rx_cb(hackrf_transfer *t)
{
    if (!running || !atomic_load(&g_hackrf_active))
        return 1;
    if (t->valid_length <= 0)
        return 0;

    int nbytes = t->valid_length;
    sample_buf_t *s = malloc(sizeof(*s) + nbytes);
    if (!s) return 0;
    s->format = SAMPLE_FMT_INT8;
    s->num = nbytes / 2;  /* 2 bytes per complex sample (int8 I + int8 Q) */
    memcpy(s->samples, t->buffer, nbytes);
    push_samples(s);
    hackrf_last_rx = time(NULL);
    hackrf_rx_active = 1;
    return 0;
}

void hackrf_request_stop(void)
{
    hackrf_device *dev = atomic_load(&g_hackrf_active);
    if (dev)
        hackrf_stop_rx(dev);
}

void *hackrf_stream_thread(void *arg)
{
    hackrf_stream_ctx_t *ctx = (hackrf_stream_ctx_t *)arg;
    const char *serial = ctx->serial;
    int first_open = 1;

    hackrf_init();

    while (running) {
        if (!first_open) {
            fprintf(stderr, "HackRF: waiting for device to reappear...\n");
            while (running) {
                if (hackrf_device_present(serial))
                    break;
                usleep((useconds_t)HACKRF_RECONNECT_SEC * 1000000);
            }
            if (!running) break;
            fprintf(stderr, "HackRF: device found, reconnecting...\n");
        }
        first_open = 0;

        hackrf_device *dev = hackrf_open_configured(serial);
        if (!dev) {
            fprintf(stderr, "HackRF: open failed, retrying in %ds...\n",
                    HACKRF_RECONNECT_SEC);
            usleep((useconds_t)HACKRF_RECONNECT_SEC * 1000000);
            continue;
        }

        hackrf_last_rx = 0;
        hackrf_rx_active = 0;

        int r = hackrf_start_rx(dev, hackrf_rx_cb, NULL);
        if (r != HACKRF_SUCCESS) {
            warnx("HackRF start_rx: %s", hackrf_error_name(r));
            hackrf_close(dev);
            usleep((useconds_t)HACKRF_RECONNECT_SEC * 1000000);
            continue;
        }

        atomic_store(&g_hackrf_active, dev);

        int lost = 0;
        while (running) {
            usleep((useconds_t)HACKRF_POLL_MS * 1000);

            r = hackrf_is_streaming(dev);
            if (r != HACKRF_TRUE) {
                fprintf(stderr, "HackRF: stream stopped (%s)\n",
                        hackrf_error_name(r));
                lost = 1;
                break;
            }

            if (hackrf_rx_active) {
                time_t now = time(NULL);
                time_t last = hackrf_last_rx;
                if (last > 0 && now - last >= HACKRF_STALL_SEC) {
                    fprintf(stderr,
                            "HackRF: no samples for %ds, assuming disconnect\n",
                            HACKRF_STALL_SEC);
                    lost = 1;
                    break;
                }
            }
        }

        hackrf_close_session(dev);

        if (!running) break;
        if (lost)
            fprintf(stderr, "HackRF: disconnected, will reconnect\n");
    }

    hackrf_exit();
    free(ctx->serial);
    free(ctx);
    return NULL;
}
