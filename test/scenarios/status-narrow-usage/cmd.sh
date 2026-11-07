#!/usr/bin/env bash
# COLUMNS=80 pins the narrow-terminal layout: the identity columns give up
# width first, then INTEGRATIONS. COST is unelidable — a cut currency reads as
# a wrong number — so it keeps its natural width and the integrations header
# absorbs the remainder instead.
COLUMNS=80 wtm status
