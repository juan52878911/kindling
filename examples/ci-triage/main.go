// Command ci-triage es el segundo ejemplo de kindling: el triaje de un fallo
// de CI en tres capas. Chispa (el modelo lineal calibrado) decide en
// microsegundos qué líneas del log explican el fallo y de qué tipo es; VON (un
// LLM pequeño en una microVM que se congela al quedarse ocioso) solo entra
// cuando Chispa duda, y solo ve el trozo elegido, no el log entero. Una
// persona confirma o corrige la categoría y eso queda en un JSONL que
// `kling chispa train` acepta tal cual. Ver README.md.
//
// Usa kindling solo por fuera: `kling chispa train|eval` para entrenar y el
// gateway de IA (`kling ai serve`) por HTTP para decidir.
package main

import (
	"fmt"
	"log"
	"os"
)

const usage = `usage: ci-triage <command> [flags]

  analyze [-gateway G] <logfile|->   triage one failed CI log
  serve   [-gateway G] [-listen A]   local web page: paste a log, see the lines that explain it, confirm the category
  data    -logchunks DIR -out DIR    build the train/valid/test sets from LogChunks
  eval    -data DIR [-gateway G]     measure the locator, the category and the baselines
  export  <feedback.jsonl>           one clean label per log, for kling chispa train or kling ai feedback -import
  lines   <logfile>                  print a log as ci-triage sees it (to annotate your own logs)

Run "ci-triage <command> -h" for its flags.
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "analyze":
		err = cmdAnalyze(args)
	case "export":
		err = cmdExport(args)
	case "lines":
		err = cmdLines(args)
	case "serve":
		err = cmdServe(args)
	case "eval":
		err = cmdEval(args)
	case "data":
		err = cmdData(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatal("ci-triage: ", err)
	}
}
