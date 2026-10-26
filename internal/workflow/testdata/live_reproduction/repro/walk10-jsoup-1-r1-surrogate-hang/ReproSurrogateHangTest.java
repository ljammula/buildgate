package org.jsoup.parser;

import org.jsoup.Jsoup;
import org.jsoup.nodes.Document;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayInputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;

import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

// With a limit of one character, an InputStream holding "a" and a
// supplementary character must still finish parsing. A hang is the
// factory's per-test timeout.
public class ReproSurrogateHangTest {
    @Test
    void limitOneWithSupplementaryCharOnStreamTerminates() throws IOException {
        Parser p = Parser.htmlParser().setMaxInputSize(1);
        byte[] in = "a😀".getBytes(StandardCharsets.UTF_8);
        Document doc = Jsoup.parse(new ByteArrayInputStream(in), "UTF-8", "", p);
        assertNotNull(doc);
        assertTrue(p.isInputTruncated());
    }
}
