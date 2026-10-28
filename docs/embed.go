// Package docs embebe la documentación madre del back en el binario: las
// instrucciones de los tres agentes expertos del flujo /agency, que el
// módulo agent parsea para armar el system prompt de cada paso.
package docs

import "embed"

// AgentMD son las instrucciones madre de los agentes del flujo /agency
// (comercial, inmobiliaria y cine), con sus secciones fijas por paso.
//
//go:embed agente-comercial.md agente-inmobiliaria.md agente-cine.md
var AgentMD embed.FS
