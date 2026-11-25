/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package v1 is the Orka oversight wire contract: the vendor-neutral record
// an oversight pipeline submits for each agent action it evaluated, and the
// read models Orka serves back.
//
// The contract names no sensor, scorer, advisor or model. Concrete plug-ins
// map their own output onto these types out of tree. The package imports
// nothing from Orka's internal packages, so a separately built pipeline can
// depend on it.
//
// Every string in a record is untrusted: it came from an agent, a sensor or a
// model. Orka redacts and bounds it on ingest, and clients must render it as
// plain text.
package v1
