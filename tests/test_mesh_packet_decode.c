/*
 * SPDX-License-Identifier: GPL-3.0-or-later
 * Copyright (c) 2026 CEMAXECUTER LLC
 *
 * Wire-format tests for mesh_packet_decode.
 *
 * mesh_packet_decode() turns a raw 16-byte radio header plus N
 * AES-CTR ciphertext bytes into the from/to/packet_id/portnum/payload
 * fields the sniffer publishes. The on-air format and nonce layout
 * come from the upstream Meshtastic firmware
 * (https://github.com/meshtastic/firmware, GPL-3.0-or-later); the
 * test vectors here are constructed locally against that spec, then
 * one golden ciphertext frame is included that was generated
 * externally with `openssl enc -aes-128-ctr` so a buggy helper can
 * not mask itself on both encode and decode sides.
 */

#include "../mesh_packet.h"
#include "../keyset.h"
#include "../meshtastic.h"

#include <openssl/evp.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>

static int fails = 0;

#define CHECK(cond, fmt, ...) do {                                    \
    if (!(cond)) {                                                    \
        fprintf(stderr, "FAIL [%s:%d]  " fmt "\n",                    \
                __FILE__, __LINE__, ##__VA_ARGS__);                   \
        fails++;                                                      \
    }                                                                 \
} while (0)

/* ---- Capture: a callback that records the first event it sees. ---- */

typedef struct {
    int             fired;
    mesh_event_t    ev;
    uint8_t         payload_buf[256];
} capture_t;

static void capture_cb(const mesh_event_t *ev, void *user)
{
    capture_t *c = (capture_t *)user;
    if (c->fired) return;
    c->fired = 1;
    c->ev    = *ev;
    if (ev->payload && ev->payload_len &&
        ev->payload_len <= sizeof(c->payload_buf)) {
        memcpy(c->payload_buf, ev->payload, ev->payload_len);
        c->ev.payload = c->payload_buf;
    }
}

/* ---- Tiny helpers ---- */

static void wr_le32(uint8_t *p, uint32_t v)
{
    p[0] = (uint8_t)(v       );
    p[1] = (uint8_t)(v >>  8);
    p[2] = (uint8_t)(v >> 16);
    p[3] = (uint8_t)(v >> 24);
}

/* Build the 16-byte radio header in place. Caller provides the rest. */
static void build_header(uint8_t hdr[16], uint32_t to, uint32_t from,
                         uint32_t packet_id, uint8_t flags,
                         uint8_t channel_hash, uint8_t next_hop,
                         uint8_t relay_node)
{
    wr_le32(hdr,     to);
    wr_le32(hdr + 4, from);
    wr_le32(hdr + 8, packet_id);
    hdr[12] = flags;
    hdr[13] = channel_hash;
    hdr[14] = next_hop;
    hdr[15] = relay_node;
}

/* Build the 16-byte AES-CTR nonce per Meshtastic spec:
 *   packet_id  (8B LE, upper 32 bits zero on-air)
 *   from_node  (4B LE)
 *   counter    (4B BE, starts at 0)
 */
static void build_nonce(uint8_t nonce[16], uint32_t packet_id, uint32_t from)
{
    memset(nonce, 0, 16);
    nonce[0] = (uint8_t)(packet_id      );
    nonce[1] = (uint8_t)(packet_id >>  8);
    nonce[2] = (uint8_t)(packet_id >> 16);
    nonce[3] = (uint8_t)(packet_id >> 24);
    nonce[8]  = (uint8_t)(from      );
    nonce[9]  = (uint8_t)(from >>  8);
    nonce[10] = (uint8_t)(from >> 16);
    nonce[11] = (uint8_t)(from >> 24);
}

/* AES-CTR encrypt using OpenSSL. key_len picks 128 vs 256. Returns
 * number of bytes written (or -1 on error). Independent of the
 * decoder so a round-trip test still proves both sides agree. */
static int aes_ctr_encrypt(const uint8_t *key, size_t key_len,
                           const uint8_t *iv,
                           const uint8_t *in, size_t in_len,
                           uint8_t *out)
{
    const EVP_CIPHER *cipher = NULL;
    if      (key_len == 16) cipher = EVP_aes_128_ctr();
    else if (key_len == 32) cipher = EVP_aes_256_ctr();
    else return -1;
    EVP_CIPHER_CTX *ctx = EVP_CIPHER_CTX_new();
    if (!ctx) return -1;
    int outlen = 0, finlen = 0, rc = -1;
    if (EVP_EncryptInit_ex(ctx, cipher, NULL, key, iv) != 1) goto done;
    if (EVP_EncryptUpdate(ctx, out, &outlen, in, (int)in_len) != 1) goto done;
    if (EVP_EncryptFinal_ex(ctx, out + outlen, &finlen) != 1) goto done;
    rc = outlen + finlen;
done:
    EVP_CIPHER_CTX_free(ctx);
    return rc;
}

/* Encrypt a Data envelope under (channel_name, psk) and produce the
 * full on-air frame in out_frame. Returns total frame length, or
 * negative on error. Caller sized out_frame for at least 16 + pt_len. */
static int build_frame(uint8_t *out_frame, size_t out_cap,
                       uint32_t to, uint32_t from, uint32_t packet_id,
                       uint8_t flags, uint8_t channel_hash,
                       const uint8_t *psk, size_t psk_len,
                       const uint8_t *pt, size_t pt_len)
{
    if (out_cap < 16 + pt_len) return -1;
    build_header(out_frame, to, from, packet_id, flags, channel_hash, 0, 0);
    uint8_t nonce[16];
    build_nonce(nonce, packet_id, from);
    int n = aes_ctr_encrypt(psk, psk_len, nonce, pt, pt_len, out_frame + 16);
    if (n < 0) return -1;
    return (int)(16 + n);
}

/* mesh_channel_hash() is declared in meshtastic.h. */

/* ============================================================== */
/* Cases                                                           */
/* ============================================================== */

static void test_header_only(void)
{
    uint8_t hdr[16];
    build_header(hdr, 0xaabbccddu, 0x11223344u, 0xdeadbeefu,
                 0x00, 0x42, 0, 0);
    capture_t cap = {0};
    int rc = mesh_packet_decode(hdr, sizeof(hdr), NULL, capture_cb, &cap);
    CHECK(rc == 0, "header-only: decode returned %d, want 0", rc);
    CHECK(cap.fired, "header-only: callback never fired");
    CHECK(cap.ev.header.to == 0xaabbccddu,
          "header-only: to=0x%08x want 0xaabbccdd", cap.ev.header.to);
    CHECK(cap.ev.header.from == 0x11223344u,
          "header-only: from=0x%08x want 0x11223344", cap.ev.header.from);
    CHECK(cap.ev.header.packet_id == 0xdeadbeefu,
          "header-only: packet_id=0x%08x want 0xdeadbeef",
          cap.ev.header.packet_id);
    CHECK(cap.ev.header.channel == 0x42,
          "header-only: channel=0x%02x want 0x42", cap.ev.header.channel);
    CHECK(cap.ev.decrypted == true,
          "header-only: decrypted=%d want true (no ciphertext to decrypt)",
          cap.ev.decrypted);
}

static void test_short_buffer_rejected(void)
{
    uint8_t buf[15] = {0};
    capture_t cap = {0};
    int rc = mesh_packet_decode(buf, sizeof(buf), NULL, capture_cb, &cap);
    CHECK(rc == -1, "short buffer: decode returned %d, want -1", rc);
    CHECK(cap.fired == 0,
          "short buffer: callback fired (%d) but frame was malformed",
          cap.fired);
}

static void test_flags_packing(void)
{
    /* hop_limit = 5 (0b101), hop_start = 6 (0b110<<5 = 0xC0), want_ack,
     * via_mqtt. Bits: HHH SSS W M -> field layout below. */
    uint8_t flags = 0;
    flags |= (5 & MESH_FLAG_HOP_LIMIT_MASK);
    flags |= (6 << MESH_FLAG_HOP_START_SHIFT) & MESH_FLAG_HOP_START_MASK;
    flags |= MESH_FLAG_WANT_ACK_MASK;
    flags |= MESH_FLAG_VIA_MQTT_MASK;

    uint8_t hdr[16];
    build_header(hdr, 0, 0, 0, flags, 0, 0, 0);
    capture_t cap = {0};
    mesh_packet_decode(hdr, sizeof(hdr), NULL, capture_cb, &cap);
    CHECK(cap.fired, "flags-packing: callback never fired");
    CHECK(cap.ev.hop_limit == 5,
          "flags-packing: hop_limit=%d want 5", cap.ev.hop_limit);
    CHECK(cap.ev.hop_start == 6,
          "flags-packing: hop_start=%d want 6", cap.ev.hop_start);
    CHECK(cap.ev.want_ack == true,
          "flags-packing: want_ack=%d want true", cap.ev.want_ack);
    CHECK(cap.ev.via_mqtt == true,
          "flags-packing: via_mqtt=%d want true", cap.ev.via_mqtt);
}

static void test_aes128_round_trip(void)
{
    /* Default LongFast: name "LongFast", PSK = MESH_DEFAULT_PSK. */
    const char *name = "LongFast";
    keyset_t *ks = keyset_create();
    keyset_add(ks, name, MESH_DEFAULT_PSK, 16);

    uint8_t pt[] = {
        0x08, 0x01,                   /* portnum varint = 1 (TEXT_MESSAGE_APP) */
        0x12, 0x05, 'h', 'e', 'l', 'l', 'o',  /* payload "hello" */
    };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0xffffffffu, 0xdeadbeefu, 0x12345678u,
                           0, mesh_channel_hash(name, MESH_DEFAULT_PSK, 16),
                           MESH_DEFAULT_PSK, 16, pt, sizeof(pt));
    CHECK(flen > 0, "AES128: build_frame failed (%d)", flen);

    capture_t cap = {0};
    int rc = mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(rc == 0, "AES128: decode rc=%d want 0", rc);
    CHECK(cap.fired, "AES128: callback never fired");
    CHECK(cap.ev.decrypted == true, "AES128: decrypted=false");
    CHECK(cap.ev.portnum == 1, "AES128: portnum=%u want 1", cap.ev.portnum);
    CHECK(cap.ev.payload_len == 5,
          "AES128: payload_len=%zu want 5", cap.ev.payload_len);
    CHECK(cap.ev.payload && memcmp(cap.ev.payload, "hello", 5) == 0,
          "AES128: payload bytes did not round-trip");
    CHECK(strcmp(cap.ev.channel_name, name) == 0,
          "AES128: channel_name=\"%s\" want \"%s\"",
          cap.ev.channel_name, name);
    keyset_destroy(ks);
}

static void test_aes256_round_trip(void)
{
    const char *name = "OpsBig";
    uint8_t psk[32];
    for (int i = 0; i < 32; ++i) psk[i] = (uint8_t)(0xa0 + i);

    keyset_t *ks = keyset_create();
    keyset_add(ks, name, psk, 32);

    uint8_t pt[] = {
        0x08, 0x42,
        0x12, 0x03, 'y', 'e', 's',
    };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0x00000001u, 0xcafebabeu, 0x00112233u,
                           0, mesh_channel_hash(name, psk, 32),
                           psk, 32, pt, sizeof(pt));
    CHECK(flen > 0, "AES256: build_frame failed (%d)", flen);

    capture_t cap = {0};
    mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(cap.fired, "AES256: callback never fired");
    CHECK(cap.ev.decrypted == true, "AES256: decrypted=false");
    CHECK(cap.ev.portnum == 0x42, "AES256: portnum=%u want 0x42",
          cap.ev.portnum);
    CHECK(cap.ev.payload_len == 3 &&
          cap.ev.payload && memcmp(cap.ev.payload, "yes", 3) == 0,
          "AES256: payload did not round-trip");
    CHECK(strcmp(cap.ev.channel_name, name) == 0,
          "AES256: channel_name=\"%s\" want \"%s\"",
          cap.ev.channel_name, name);
    keyset_destroy(ks);
}

static void test_data_envelope_extras(void)
{
    /* Exercise want_response (bool varint, field 3),
     * request_id (fixed32, field 6), reply_id (fixed32, field 7). */
    const char *name = "LongFast";
    keyset_t *ks = keyset_create();
    keyset_add(ks, name, MESH_DEFAULT_PSK, 16);

    uint8_t pt[] = {
        0x08, 0x01,                                       /* portnum=1 */
        0x12, 0x02, 'h', 'i',                             /* payload "hi" */
        0x18, 0x01,                                       /* want_response=true */
        0x35, 0x11, 0x22, 0x33, 0x44,                     /* request_id=0x44332211 (fixed32, field 6) */
        0x3d, 0xaa, 0xbb, 0xcc, 0xdd,                     /* reply_id=0xddccbbaa  (fixed32, field 7) */
    };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0, 0xdeadbeefu, 0x12345678u,
                           0, mesh_channel_hash(name, MESH_DEFAULT_PSK, 16),
                           MESH_DEFAULT_PSK, 16, pt, sizeof(pt));
    CHECK(flen > 0, "envelope-extras: build failed");

    capture_t cap = {0};
    mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(cap.fired && cap.ev.decrypted, "envelope-extras: decrypt failed");
    CHECK(cap.ev.want_response == true,
          "envelope-extras: want_response=%d want true",
          cap.ev.want_response);
    CHECK(cap.ev.request_id == 0x44332211u,
          "envelope-extras: request_id=0x%08x want 0x44332211",
          cap.ev.request_id);
    CHECK(cap.ev.reply_id == 0xddccbbaau,
          "envelope-extras: reply_id=0x%08x want 0xddccbbaa",
          cap.ev.reply_id);
    keyset_destroy(ks);
}

static void test_wrong_psk(void)
{
    /* Encrypt under one PSK, decrypt-attempt against a different one
     * that happens to share the channel-hash byte. Header still
     * parses; decrypted must be false. */
    const char *name = "Ops";
    uint8_t real_psk[16];  for (int i = 0; i < 16; ++i) real_psk[i] = (uint8_t)i;
    uint8_t fake_psk[16];  for (int i = 0; i < 16; ++i) fake_psk[i] = (uint8_t)(0xff - i);
    uint8_t ch = mesh_channel_hash(name, real_psk, 16);

    keyset_t *ks = keyset_create();
    keyset_add_raw(ks, ch, fake_psk, 16, name);

    uint8_t pt[] = { 0x08, 0x01, 0x12, 0x02, 'h', 'i' };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0, 0xdeadbeefu, 0x12345678u, 0, ch,
                           real_psk, 16, pt, sizeof(pt));
    CHECK(flen > 0, "wrong-PSK: build failed");

    capture_t cap = {0};
    mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(cap.fired, "wrong-PSK: callback never fired");
    CHECK(cap.ev.decrypted == false,
          "wrong-PSK: decrypted=%d want false", cap.ev.decrypted);
    CHECK(cap.ev.header.from == 0xdeadbeefu,
          "wrong-PSK: header still expected from=0xdeadbeef, got 0x%08x",
          cap.ev.header.from);
    keyset_destroy(ks);
}

static void test_unknown_channel_hash(void)
{
    /* Empty keyset: bucket lookup yields nothing, header parses,
     * decrypted=false. */
    keyset_t *ks = keyset_create();
    uint8_t pt[]   = { 0xaa, 0xbb };  /* random ciphertext stand-in */
    uint8_t frame[18];
    build_header(frame, 0, 0xdeadbeefu, 0x12345678u, 0, 0x7e, 0, 0);
    memcpy(frame + 16, pt, sizeof(pt));

    capture_t cap = {0};
    int rc = mesh_packet_decode(frame, sizeof(frame), ks, capture_cb, &cap);
    CHECK(rc == 0, "unknown-hash: rc=%d want 0", rc);
    CHECK(cap.fired, "unknown-hash: callback never fired");
    CHECK(cap.ev.decrypted == false,
          "unknown-hash: decrypted=%d want false (no key)",
          cap.ev.decrypted);
    CHECK(cap.ev.header.channel == 0x7e,
          "unknown-hash: channel=0x%02x want 0x7e", cap.ev.header.channel);
    keyset_destroy(ks);
}

static void test_radio_preset_resolution(void)
{
    /* mesh_packet_decode_with_radio resolves preset_name from
     * (sf, cr, bw_hz). LongFast = (11, 5, 250000). */
    uint8_t hdr[16];
    build_header(hdr, 0, 0xdeadbeefu, 0x1u, 0, 0, 0, 0);
    capture_t cap = {0};
    int rc = mesh_packet_decode_with_radio(hdr, sizeof(hdr),
                                           -7.5f, 9.0f,
                                           11, 5, 250000,
                                           NULL, capture_cb, &cap);
    CHECK(rc == 0, "radio-preset: rc=%d want 0", rc);
    CHECK(cap.fired, "radio-preset: callback never fired");
    CHECK(strcmp(cap.ev.preset_name, "LongFast") == 0,
          "radio-preset: preset_name=\"%s\" want \"LongFast\"",
          cap.ev.preset_name);
    CHECK(cap.ev.sf == 11 && cap.ev.cr == 5 && cap.ev.bw_hz == 250000,
          "radio-preset: radio fields (sf=%d cr=%d bw=%d) not threaded",
          cap.ev.sf, cap.ev.cr, cap.ev.bw_hz);
    CHECK(cap.ev.snr_db == 9.0f && cap.ev.rssi_db == -7.5f,
          "radio-preset: rssi/snr (%.1f / %.1f) not threaded",
          cap.ev.rssi_db, cap.ev.snr_db);
}

static void test_unknown_data_port(void)
{
    /* Well-formed Data envelope on an unknown port (e.g. 999). Bytes
     * should still surface; decrypted=true; downstream consumers
     * decide what to do with an unhandled portnum. Protects the
     * "raw but trusted" path. */
    const char *name = "LongFast";
    keyset_t *ks = keyset_create();
    keyset_add(ks, name, MESH_DEFAULT_PSK, 16);

    uint8_t pt[] = {
        0x08, 0xe7, 0x07,             /* portnum varint = 999 */
        0x12, 0x04, 0xde, 0xad, 0xbe, 0xef,
    };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0, 0xdeadbeefu, 0x12345678u,
                           0, mesh_channel_hash(name, MESH_DEFAULT_PSK, 16),
                           MESH_DEFAULT_PSK, 16, pt, sizeof(pt));
    CHECK(flen > 0, "unknown-port: build failed");

    capture_t cap = {0};
    mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(cap.fired && cap.ev.decrypted,
          "unknown-port: did not decrypt (fired=%d decrypted=%d)",
          cap.fired, cap.ev.decrypted);
    CHECK(cap.ev.portnum == 999,
          "unknown-port: portnum=%u want 999", cap.ev.portnum);
    CHECK(cap.ev.payload_len == 4,
          "unknown-port: payload_len=%zu want 4", cap.ev.payload_len);
    static const uint8_t want_bytes[4] = { 0xde, 0xad, 0xbe, 0xef };
    CHECK(cap.ev.payload &&
          memcmp(cap.ev.payload, want_bytes, 4) == 0,
          "unknown-port: payload bytes did not survive");
    keyset_destroy(ks);
}

static void test_malformed_data_after_decrypt(void)
{
    /* Successful AES-CTR decrypt produces bytes that look like a
     * Data envelope but the inner parser rejects them. The decoder
     * walks the keyset, no candidate parses, and emits a final
     * undecrypted event so downstream still sees the header. */
    const char *name = "LongFast";
    keyset_t *ks = keyset_create();
    keyset_add(ks, name, MESH_DEFAULT_PSK, 16);

    /* Tag with field=1, wire-type=2 (length-delimited), then a length
     * larger than the buffer. parse_data_envelope's pb_read_length()
     * fails immediately. */
    uint8_t pt[] = { 0x0a, 0x40, 0xde, 0xad };
    uint8_t frame[64];
    int flen = build_frame(frame, sizeof(frame),
                           0, 0xdeadbeefu, 0x12345678u,
                           0, mesh_channel_hash(name, MESH_DEFAULT_PSK, 16),
                           MESH_DEFAULT_PSK, 16, pt, sizeof(pt));
    CHECK(flen > 0, "malformed: build failed");

    capture_t cap = {0};
    mesh_packet_decode(frame, (size_t)flen, ks, capture_cb, &cap);
    CHECK(cap.fired, "malformed: callback never fired");
    CHECK(cap.ev.decrypted == false,
          "malformed: decrypted=%d want false (parser rejected envelope)",
          cap.ev.decrypted);
    CHECK(cap.ev.header.from == 0xdeadbeefu,
          "malformed: header still expected, got from=0x%08x",
          cap.ev.header.from);
    keyset_destroy(ks);
}

static void test_golden_aes128_ciphertext(void)
{
    /* Fixed reference vector. Generated externally with
     *   openssl enc -aes-128-ctr -K <MESH_DEFAULT_PSK> -iv <nonce>
     * where:
     *   plaintext = 08 01 12 02 68 69   (port=1=TEXT_MESSAGE_APP, payload="hi")
     *   nonce     = 78 56 34 12 00 00 00 00 ef be ad de 00 00 00 00
     *               (packet_id=0x12345678 LE 8B | from=0xdeadbeef LE 4B
     *                | counter BE 4B = 0)
     *   key       = MESH_DEFAULT_PSK
     * Produces ciphertext = ab a2 29 93 ee 1d.
     *
     * Having this here guards against a same-bug-on-both-sides round
     * trip in test_aes128_round_trip: if our build_nonce / decoder
     * drift, this test fails even if the round trip still "passes". */
    const char *name = "LongFast";
    keyset_t *ks = keyset_create();
    keyset_add(ks, name, MESH_DEFAULT_PSK, 16);

    uint8_t frame[22];
    build_header(frame, 0xffffffffu, 0xdeadbeefu, 0x12345678u,
                 0, mesh_channel_hash(name, MESH_DEFAULT_PSK, 16), 0, 0);
    static const uint8_t golden_ct[6] = {
        0xab, 0xa2, 0x29, 0x93, 0xee, 0x1d
    };
    memcpy(frame + 16, golden_ct, 6);

    capture_t cap = {0};
    int rc = mesh_packet_decode(frame, sizeof(frame), ks, capture_cb, &cap);
    CHECK(rc == 0, "golden: rc=%d want 0", rc);
    CHECK(cap.fired && cap.ev.decrypted,
          "golden: did not decrypt (fired=%d decrypted=%d)",
          cap.fired, cap.ev.decrypted);
    CHECK(cap.ev.portnum == 1,
          "golden: portnum=%u want 1", cap.ev.portnum);
    CHECK(cap.ev.payload_len == 2 &&
          cap.ev.payload && memcmp(cap.ev.payload, "hi", 2) == 0,
          "golden: payload did not match (len=%zu)", cap.ev.payload_len);
    keyset_destroy(ks);
}

int main(void)
{
    test_header_only();
    test_short_buffer_rejected();
    test_flags_packing();
    test_aes128_round_trip();
    test_aes256_round_trip();
    test_data_envelope_extras();
    test_wrong_psk();
    test_unknown_channel_hash();
    test_radio_preset_resolution();
    test_unknown_data_port();
    test_malformed_data_after_decrypt();
    test_golden_aes128_ciphertext();

    if (fails == 0) { printf("OK\n"); return 0; }
    fprintf(stderr, "%d test(s) failed\n", fails);
    return 1;
}
