package agent

import "strings"

// convertLatex replaces common LaTeX math expressions with their Unicode
// equivalents. Only the $\cmd$ inline form is substituted; arbitrary inline
// math ($...$) is left unchanged to avoid false-positive substitutions (e.g.
// dollar amounts like "$100").
//
// This is applied as part of sanitizeOutput so IM platforms receive readable
// Unicode text instead of raw LaTeX commands.
func convertLatex(s string) string {
	for _, r := range latexTable {
		s = strings.ReplaceAll(s, r[0], r[1])
	}
	return s
}

// latexTable lists ($\cmd$, unicode) pairs in substitution order.
// Longer/more-specific commands are listed before shorter prefixes to avoid
// partial replacements (e.g. \Gamma before \gamma if they shared a prefix).
var latexTable = [][2]string{
	// ── Arrows ───────────────────────────────────────────────────────────────
	{`$\leftrightarrow$`, `↔`},
	{`$\Leftrightarrow$`, `⇔`},
	{`$\rightarrow$`, `→`},
	{`$\leftarrow$`, `←`},
	{`$\Rightarrow$`, `⇒`},
	{`$\Leftarrow$`, `⇐`},
	{`$\uparrow$`, `↑`},
	{`$\downarrow$`, `↓`},
	{`$\nearrow$`, `↗`},
	{`$\searrow$`, `↘`},
	{`$\nwarrow$`, `↖`},
	{`$\swarrow$`, `↙`},
	{`$\mapsto$`, `↦`},
	{`$\to$`, `→`},
	{`$\gets$`, `←`},

	// ── Comparison ───────────────────────────────────────────────────────────
	{`$\approx$`, `≈`},
	{`$\simeq$`, `≃`},
	{`$\sim$`, `∼`},
	{`$\cong$`, `≅`},
	{`$\equiv$`, `≡`},
	{`$\neq$`, `≠`},
	{`$\ne$`, `≠`},
	{`$\geq$`, `≥`},
	{`$\ge$`, `≥`},
	{`$\leq$`, `≤`},
	{`$\le$`, `≤`},
	{`$\gg$`, `≫`},
	{`$\ll$`, `≪`},
	{`$\propto$`, `∝`},

	// ── Arithmetic ───────────────────────────────────────────────────────────
	{`$\times$`, `×`},
	{`$\div$`, `÷`},
	{`$\cdot$`, `·`},
	{`$\pm$`, `±`},
	{`$\mp$`, `∓`},
	{`$\infty$`, `∞`},
	{`$\partial$`, `∂`},
	{`$\nabla$`, `∇`},
	{`$\sqrt{}$`, `√`},
	{`$\circ$`, `∘`},

	// ── Sets and logic ───────────────────────────────────────────────────────
	{`$\subseteq$`, `⊆`},
	{`$\supseteq$`, `⊇`},
	{`$\subset$`, `⊂`},
	{`$\supset$`, `⊃`},
	{`$\notin$`, `∉`},
	{`$\in$`, `∈`},
	{`$\cup$`, `∪`},
	{`$\cap$`, `∩`},
	{`$\emptyset$`, `∅`},
	{`$\varnothing$`, `∅`},
	{`$\forall$`, `∀`},
	{`$\exists$`, `∃`},
	{`$\nexists$`, `∄`},
	{`$\lnot$`, `¬`},
	{`$\neg$`, `¬`},
	{`$\wedge$`, `∧`},
	{`$\land$`, `∧`},
	{`$\vee$`, `∨`},
	{`$\lor$`, `∨`},
	{`$\oplus$`, `⊕`},
	{`$\otimes$`, `⊗`},

	// ── Calculus / operators ─────────────────────────────────────────────────
	{`$\sum$`, `∑`},
	{`$\prod$`, `∏`},
	{`$\int$`, `∫`},
	{`$\iint$`, `∬`},
	{`$\iiint$`, `∭`},

	// ── Greek uppercase (before lowercase to avoid prefix clashes) ───────────
	{`$\Gamma$`, `Γ`},
	{`$\Delta$`, `Δ`},
	{`$\Theta$`, `Θ`},
	{`$\Lambda$`, `Λ`},
	{`$\Xi$`, `Ξ`},
	{`$\Pi$`, `Π`},
	{`$\Sigma$`, `Σ`},
	{`$\Upsilon$`, `Υ`},
	{`$\Phi$`, `Φ`},
	{`$\Psi$`, `Ψ`},
	{`$\Omega$`, `Ω`},

	// ── Greek lowercase ───────────────────────────────────────────────────────
	{`$\alpha$`, `α`},
	{`$\beta$`, `β`},
	{`$\gamma$`, `γ`},
	{`$\delta$`, `δ`},
	{`$\varepsilon$`, `ε`},
	{`$\epsilon$`, `ε`},
	{`$\zeta$`, `ζ`},
	{`$\eta$`, `η`},
	{`$\theta$`, `θ`},
	{`$\vartheta$`, `θ`},
	{`$\iota$`, `ι`},
	{`$\kappa$`, `κ`},
	{`$\lambda$`, `λ`},
	{`$\mu$`, `μ`},
	{`$\nu$`, `ν`},
	{`$\xi$`, `ξ`},
	{`$\pi$`, `π`},
	{`$\varpi$`, `ϖ`},
	{`$\rho$`, `ρ`},
	{`$\varrho$`, `ϱ`},
	{`$\sigma$`, `σ`},
	{`$\varsigma$`, `ς`},
	{`$\tau$`, `τ`},
	{`$\upsilon$`, `υ`},
	{`$\varphi$`, `φ`},
	{`$\phi$`, `φ`},
	{`$\chi$`, `χ`},
	{`$\psi$`, `ψ`},
	{`$\omega$`, `ω`},

	// ── Misc symbols ──────────────────────────────────────────────────────────
	{`$\ell$`, `ℓ`},
	{`$\hbar$`, `ℏ`},
	{`$\Re$`, `ℜ`},
	{`$\Im$`, `ℑ`},
	{`$\aleph$`, `ℵ`},
	{`$\dagger$`, `†`},
	{`$\ddagger$`, `‡`},
	{`$\star$`, `⋆`},
	{`$\bullet$`, `•`},
	{`$\cdots$`, `⋯`},
	{`$\ldots$`, `…`},
	{`$\dots$`, `…`},
	{`$\therefore$`, `∴`},
	{`$\because$`, `∵`},
	{`$\angle$`, `∠`},
	{`$\perp$`, `⊥`},
	{`$\parallel$`, `∥`},
}
