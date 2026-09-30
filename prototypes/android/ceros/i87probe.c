// i87probe — sondas en el invitado para el issue #87 (páginas a ceros).
//
//   i87probe canary MIB INTERVALO_S DURACION_S
//       reserva MIB MiB anónimos, pone en cada página de 4 KiB un patrón no
//       nulo que depende de su índice, y los comprueba cada INTERVALO_S s.
//       Cada página mala: índice, PFN, si está entera a ceros, y cuáles de las
//       otras 3 páginas de su grupo de 16 KiB (PFN & ~3, una página del host)
//       son también canarios y si están malas. Reescribe la mala y sigue.
//   i87probe pfn FICHERO PAGINA...
//       mapea el fichero, toca esas páginas y dice su PFN, kpageflags y
//       kpagecount, y los de las otras 3 páginas de su grupo de 16 KiB.
#define _GNU_SOURCE
#include <fcntl.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <time.h>
#include <unistd.h>

#define PG 4096UL
#define W (PG / 8)

static int pm_fd = -1, kf_fd = -1, kc_fd = -1, km_fd = -1;
static char linea[4096]; static size_t ll;
// ap: acumula una línea; kmsg: la manda también a /dev/kmsg (sobrevive en la consola si el invitado muere)
#define ap(...) (ll += snprintf(linea + ll, sizeof linea - ll, __VA_ARGS__))
static void kmsg(void) { fputs(linea, stdout); fflush(stdout); if (km_fd >= 0) { char b[4200]; int n = snprintf(b, sizeof b, "<3>%s", linea); if (write(km_fd, b, n) < 0) {} } ll = 0; linea[0] = 0; }

static uint64_t pfn_of(void *p) {
	uint64_t e = 0;
	if (pread(pm_fd, &e, 8, ((uintptr_t)p / PG) * 8) != 8) return 0;
	if (!(e >> 63)) return 0; // no presente
	return e & ((1ULL << 55) - 1);
}
static uint64_t rd64(int fd, uint64_t pfn) {
	uint64_t v = ~0ULL;
	if (pread(fd, &v, 8, pfn * 8) != 8) return ~0ULL;
	return v;
}
static void flags_str(uint64_t f, char *out) {
	static const char *n[] = {"locked", "error", "referenced", "uptodate", "dirty", "lru", "active", "slab",
	                          "writeback", "reclaim", "buddy", "mmap", "anon", "swapcache", "swapbacked",
	                          "compound_head", "compound_tail", "huge", "unevictable", "hwpoison", "nopage",
	                          "ksm", "thp", "offline", "zero_page", "idle", "pgtable"};
	out[0] = 0;
	if (f == ~0ULL) { strcpy(out, "?"); return; }
	for (int i = 0; i < 27; i++)
		if (f & (1ULL << i)) { strcat(out, n[i]); strcat(out, ","); }
}
static double now(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return t.tv_sec + t.tv_nsec / 1e9;
}
static const char *reloj(void) {
	static char b[32]; time_t t = time(NULL); struct tm tm; localtime_r(&t, &tm);
	strftime(b, sizeof b, "%H:%M:%S", &tm); return b;
}
static uint64_t patron(size_t pg, size_t w) { return 0x8700000000000001ULL ^ ((uint64_t)pg << 12) ^ (w << 1); }

static int canary(size_t mib, int intervalo, int duracion) {
	size_t n = mib * 1024 * 1024 / PG;
	uint64_t *m = mmap(NULL, n * PG, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_POPULATE, -1, 0);
	if (m == MAP_FAILED) { perror("mmap"); return 1; }
	madvise(m, n * PG, MADV_NOHUGEPAGE);
	for (size_t i = 0; i < n; i++)
		for (size_t w = 0; w < W; w++) m[i * W + w] = patron(i, w);
	// PFN → índice, para mirar los vecinos del grupo de 16 KiB
	uint64_t *pfn = calloc(n, 8);
	uint64_t maxp = 0;
	for (size_t i = 0; i < n; i++) { pfn[i] = pfn_of(&m[i * W]); if (pfn[i] > maxp) maxp = pfn[i]; }
	int32_t *idx = malloc((maxp + 4) * 4);
	memset(idx, 0xff, (maxp + 4) * 4);
	for (size_t i = 0; i < n; i++) if (pfn[i]) idx[pfn[i]] = (int32_t)i;
	size_t grupos_llenos = 0;
	for (uint64_t g = 0; g + 3 <= maxp; g += 4)
		if (idx[g] >= 0 && idx[g + 1] >= 0 && idx[g + 2] >= 0 && idx[g + 3] >= 0) grupos_llenos++;
	ap("canary: %zu pages (%zu MiB), %zu complete 16K groups\n", n, mib, grupos_llenos);
	kmsg();
	double t0 = now();
	unsigned long vueltas = 0, malas = 0;
	char fs[256];
	while (now() - t0 < duracion) {
		sleep(intervalo);
		vueltas++;
		for (size_t i = 0; i < n; i++) {
			uint64_t *p = &m[i * W];
			size_t dif = 0, nz = 0;
			for (size_t w = 0; w < W; w++) {
				if (p[w] != patron(i, w)) dif++;
				if (p[w]) nz++;
			}
			if (!dif) continue;
			malas++;
			uint64_t f = pfn_of(p);
			flags_str(rd64(kf_fd, f), fs);
			ap("CANARY_BAD wall=%s t=%.0f round=%lu page=%zu pfn=0x%lx (was 0x%lx) words_bad=%zu nonzero_words=%zu flags=%s group:",
			   reloj(), now() - t0, vueltas, i, (unsigned long)f, (unsigned long)pfn[i], dif, nz, fs);
			for (uint64_t q = f & ~3ULL; q < (f & ~3ULL) + 4; q++) {
				if (q == f) continue;
				int32_t j = q <= maxp ? idx[q] : -1;
				if (j < 0) { flags_str(rd64(kf_fd, q), fs); ap(" [0x%lx other:%s]", (unsigned long)q, fs); continue; }
				size_t d2 = 0, z2 = 0;
				for (size_t w = 0; w < W; w++) { if (m[j * W + w] != patron(j, w)) d2++; if (!m[j * W + w]) z2++; }
				ap(" [0x%lx canary %d bad=%zu zero=%zu]", (unsigned long)q, j, d2, z2);
			}
			ap("\n");
			kmsg();
			for (size_t w = 0; w < W; w++) p[w] = patron(i, w);
		}
	}
	printf("canary: %lu rounds, %lu bad pages\n", vueltas, malas);
	return 0;
}

static int pfn_cmd(const char *path, int argc, char **argv) {
	int fd = open(path, O_RDONLY);
	if (fd < 0) { perror(path); return 1; }
	off_t sz = lseek(fd, 0, SEEK_END);
	unsigned char *m = mmap(NULL, sz, PROT_READ, MAP_SHARED, fd, 0);
	if (m == MAP_FAILED) { perror("mmap"); return 1; }
	char fs[256];
	for (int a = 0; a < argc; a++) {
		size_t pg = strtoul(argv[a], NULL, 0);
		if ((off_t)(pg * PG) >= sz) continue;
		volatile unsigned char c = m[pg * PG]; (void)c;
		size_t nz = 0;
		for (size_t b = 0; b < PG; b++) nz += m[pg * PG + b] != 0;
		uint64_t f = pfn_of(&m[pg * PG]);
		printf("page %zu nonzero_bytes=%zu pfn=0x%lx", pg, nz, (unsigned long)f);
		for (uint64_t q = f & ~3ULL; q < (f & ~3ULL) + 4; q++) {
			flags_str(rd64(kf_fd, q), fs);
			printf(" [0x%lx%s cnt=%ld %s]", (unsigned long)q, q == f ? "*" : "", (long)rd64(kc_fd, q), fs);
		}
		printf("\n");
	}
	return 0;
}

int main(int argc, char **argv) {
	pm_fd = open("/proc/self/pagemap", O_RDONLY);
	kf_fd = open("/proc/kpageflags", O_RDONLY);
	kc_fd = open("/proc/kpagecount", O_RDONLY);
	km_fd = open("/dev/kmsg", O_WRONLY);
	if (argc >= 5 && !strcmp(argv[1], "canary")) return canary(strtoul(argv[2], NULL, 0), atoi(argv[3]), atoi(argv[4]));
	if (argc >= 4 && !strcmp(argv[1], "pfn")) return pfn_cmd(argv[2], argc - 3, argv + 3);
	fprintf(stderr, "usage: i87probe canary MIB INTERVAL_S DURATION_S | pfn FILE PAGE...\n");
	return 2;
}
