// Fixture source for the GNU build-id normalisation tests.
// Deliberately tiny, but real: it produces real .text, .rodata and relocations,
// so a byte flipped inside .text lands in genuine executable code.
#include <stdint.h>

int32_t soroq_fixture_add(int32_t a, int32_t b) { return a + b; }

int32_t soroq_fixture_mul(int32_t a, int32_t b) { return a * b; }

const char *soroq_fixture_tag(void) { return "soroq-native-build-id-fixture"; }
