// tocar: el programa del invitado de prueba de los spikes de la ronda 9 (prototypes/r9-*).
// No es código de producto: se compila estático en el host del banco (gcc -static) y va en el rootfs
// de prueba que arma lib-fc.sh.
//
//   tocar cargar <origen> <destino>   copia origen (el disco de datos) a destino (un fichero de un
//                                     tmpfs: RAM del invitado, como los pesos que llama-server lee con
//                                     O_DIRECT a memoria anónima) e imprime "R9 cargar ms=… bytes=…"
//   tocar bucle <fichero>             mapea el fichero de solo lectura y, por cada línea de la entrada
//                                     estándar ("t"), lee un byte de cada página de 4 KiB e imprime
//                                     "R9 tocar n=… ms=… paginas=…"; "q" sale. Tras restaurar un
//                                     snapshot, la primera pasada paga los fallos de EPT del host.
#define _GNU_SOURCE
#include <fcntl.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <time.h>
#include <unistd.h>

static double ahora_ms(void) {
	struct timespec t;
	clock_gettime(CLOCK_MONOTONIC, &t);
	return t.tv_sec * 1e3 + t.tv_nsec / 1e6;
}

static int cargar(const char *src, const char *dst) {
	int in = open(src, O_RDONLY), out = open(dst, O_WRONLY | O_CREAT | O_TRUNC, 0600);
	if (in < 0 || out < 0) {
		perror("open");
		return 1;
	}
	size_t tam = 4 << 20;
	char *buf = malloc(tam);
	long long total = 0;
	double t0 = ahora_ms();
	for (;;) {
		ssize_t n = read(in, buf, tam);
		if (n < 0) {
			perror("read");
			return 1;
		}
		if (n == 0)
			break;
		for (ssize_t o = 0; o < n;) {
			ssize_t w = write(out, buf + o, n - o);
			if (w <= 0) {
				perror("write");
				return 1;
			}
			o += w;
		}
		total += n;
	}
	close(out);
	printf("R9 cargar ms=%.1f bytes=%lld\n", ahora_ms() - t0, total);
	fflush(stdout);
	return 0;
}

static int bucle(const char *f) {
	int fd = open(f, O_RDONLY);
	struct stat st;
	if (fd < 0 || fstat(fd, &st) < 0) {
		perror(f);
		return 1;
	}
	volatile unsigned char *p = mmap(NULL, st.st_size, PROT_READ, MAP_SHARED, fd, 0);
	if (p == MAP_FAILED) {
		perror("mmap");
		return 1;
	}
	printf("R9 listo bytes=%lld\n", (long long)st.st_size);
	fflush(stdout);
	char linea[64];
	int n = 0;
	while (fgets(linea, sizeof linea, stdin)) {
		if (linea[0] == 'q')
			break;
		if (linea[0] != 't')
			continue;
		unsigned long suma = 0;
		long paginas = 0;
		double t0 = ahora_ms();
		for (off_t o = 0; o < st.st_size; o += 4096, paginas++)
			suma += p[o];
		printf("R9 tocar n=%d ms=%.1f paginas=%ld suma=%lu\n", ++n, ahora_ms() - t0, paginas, suma);
		fflush(stdout);
	}
	return 0;
}

int main(int argc, char **argv) {
	if (argc == 4 && strcmp(argv[1], "cargar") == 0)
		return cargar(argv[2], argv[3]);
	if (argc == 3 && strcmp(argv[1], "bucle") == 0)
		return bucle(argv[2]);
	fprintf(stderr, "uso: tocar cargar <origen> <destino> | tocar bucle <fichero>\n");
	return 2;
}
