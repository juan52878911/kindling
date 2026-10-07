//! kling-unpack descomprime una capa OCI de stdin a stdout.
//!
//! Es el camino rápido de `decompress` en internal/oci: el compress/gzip de Go
//! va a ~200 MB/s y en un import grande es lo que más tarda. Aquí gzip va con
//! zlib-rs y zstd con libzstd. El formato lo deciden los primeros bytes, igual
//! que en Go: hay herramientas que suben capas zstd etiquetadas como gzip.
//!
//! El contrato con Go es estrecho a propósito: sin argumentos, sin entorno,
//! sale con 0 solo si la entrada entera era un flujo válido (CRC32 de cada
//! miembro gzip, xxhash64 de cada marco zstd que lo lleve) y escribió todo lo
//! descomprimido. Cualquier otra cosa sale con 1 y una línea en stderr. La
//! capa ya llega verificada por sha256; esto no la verifica, la abre.
//!
//! La entrada es de un registro: no se confía en ella. Los topes de memoria
//! son los de Go: ventana de zstd de 128 MiB como mucho (window_log_max 27).

use std::io::{self, BufRead, BufReader, BufWriter, ErrorKind, Read, Write};
use std::process::ExitCode;

const ZSTD_MAGIC: [u8; 4] = [0x28, 0xb5, 0x2f, 0xfd];
const BUF: usize = 1 << 20;

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        // Go cerró la tubería antes de acabar (cancelado o falló otra capa):
        // no es un error de la capa.
        Err(e) if e.kind() == ErrorKind::BrokenPipe => ExitCode::from(1),
        Err(e) => {
            let _ = writeln!(io::stderr(), "kling-unpack: {e}");
            ExitCode::from(1)
        }
    }
}

fn run() -> io::Result<()> {
    if std::env::args_os().len() > 1 {
        return Err(io::Error::new(
            ErrorKind::InvalidInput,
            "takes no arguments: stdin to stdout",
        ));
    }
    let mut input = BufReader::with_capacity(BUF, io::stdin().lock());
    let out = BufWriter::with_capacity(BUF, io::stdout().lock());
    let head = input.fill_buf()?;
    if head.len() >= 2 && head[0] == 0x1f && head[1] == 0x8b {
        // Multimiembro, como gzip.Reader de Go: un gzip partido en trozos
        // concatenados es una capa válida.
        copy(flate2::bufread::MultiGzDecoder::new(input), out)
    } else if head.len() >= 4 && head[..4] == ZSTD_MAGIC {
        // Varios marcos seguidos (y los saltables) los encadena libzstd.
        let mut d = zstd::stream::read::Decoder::with_buffer(input)?;
        d.window_log_max(27)?;
        copy(d, out)
    } else {
        Err(io::Error::new(
            ErrorKind::InvalidData,
            "compressed layer is neither gzip nor zstd",
        ))
    }
}

fn copy<R: Read, W: Write>(mut r: R, mut w: W) -> io::Result<()> {
    io::copy(&mut r, &mut w)?;
    w.flush()
}
