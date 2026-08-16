# Project-specific knobs for scripts/lint-docs.py.
#
# The linter beside this file is vendored and stays byte-identical across
# projects. Everything that differs between them lives here, so a fix to a check
# can be copied out again without carrying one project's vocabulary into the
# next. Tune until every reported problem is a real one: a check that cries wolf
# is worse than no check.

# Directories holding fixtures, corpora or generated Markdown, and root-level
# .md files that are data rather than documentation. Added to the linter's own
# list, which already covers node_modules, vendor, dist, bin, target, build,
# third_party, testdata and CHANGELOG.md.
SKIP_EXTRA = set()

# The domain terms a reader of this project cannot infer. Each must be
# introduced where a document first uses it: glossed on the spot, defined in a
# glossary table, or linked to the page that defines it.
GLOSSARY = [
    "cassette", "step", "surface", "alignment", "volatile", "canonical",
    "normalize", "drift", "provenance", "ruleset",
]

# Words that legitimately start a sentence in lower case, which is nearly always
# the project's own command name. Without them the splitter reads "Nothing
# matches. cs-vcr exits 1." as one 6-word sentence and reports a length that is
# not real.
LOWERCASE_STARTERS = ["cs-vcr"]

# Verbs the shared list does not carry, added when a real verb trips the epigram
# check. Regex fragments rather than literals, so "mints?" covers both numbers.
# Only what is this project's own belongs here: an ordinary English verb should
# go into SHARED_VERBS in the linter and be vendored back out to everyone.
PROJECT_VERBS = []
