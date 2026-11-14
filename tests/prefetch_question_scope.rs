use noema::{
    cortex::Cortex,
    prefetch::{PrefetchOptions, PrefetchResult, retrieve},
    trace::Trace,
};

struct Fixture {
    _temp: tempfile::TempDir,
    cortex: Cortex,
}

impl Fixture {
    fn new() -> Self {
        let temp = tempfile::tempdir().unwrap();
        Cortex::create("question-scope", temp.path()).unwrap();
        let cortex = Cortex::open("question-scope", temp.path().join("question-scope")).unwrap();
        Self {
            _temp: temp,
            cortex,
        }
    }

    fn add(&self, title: &str, body: &str) {
        let mut trace = Trace::new(title, "fact", "test", vec![], body);
        self.cortex.add(&mut trace).unwrap();
    }

    fn retrieve(&self, prompt: &str) -> PrefetchResult {
        self.retrieve_with(prompt, options())
    }

    fn retrieve_with(&self, prompt: &str, options: PrefetchOptions) -> PrefetchResult {
        retrieve(&self.cortex, prompt, options).unwrap()
    }
}

fn options() -> PrefetchOptions {
    PrefetchOptions {
        max_preferences: 0,
        ..PrefetchOptions::default()
    }
}

fn assert_markers(result: &PrefetchResult, present: &[&str], absent: &[&str]) {
    for marker in present {
        assert!(
            result.context.contains(marker),
            "missing {marker:?} from retrieved context: {:?}",
            result.context
        );
    }
    for marker in absent {
        assert!(
            !result.context.contains(marker),
            "unexpected {marker:?} in retrieved context: {:?}",
            result.context
        );
    }
}

fn ferry_fixture() -> Fixture {
    let fixture = Fixture::new();
    fixture.add(
        "Juniper ferry boarding gate",
        "The Juniper ferry boarding gate is the east arch. GATE_EAST_ARCH",
    );
    fixture.add(
        "Juniper ferry seating chart",
        "The Juniper ferry seating chart places families upstairs. SEATING_UPSTAIRS",
    );
    fixture.add(
        "Cedar railway boarding gate",
        "The Cedar railway boarding gate is the west ramp. RAILWAY_WEST_RAMP",
    );
    fixture
}

#[test]
fn literal_and_short_question_retrieve_the_answer_not_nearby_topics() {
    let fixture = ferry_fixture();
    for prompt in [
        "Juniper ferry boarding gate",
        "What is the Juniper ferry boarding gate?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &["GATE_EAST_ARCH"],
            &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 1, "prompt: {prompt}");
    }
}

#[test]
fn unrelated_long_prefix_and_suffix_do_not_replace_the_question() {
    let fixture = ferry_fixture();
    let background = "The pottery workshop has shelves of glazed bowls beside the drying room. \
        Volunteers are arranging paint brushes, checking linen aprons, sorting wooden tools, \
        and preparing a calendar for the weekend exhibition. These workshop details are background.";
    for prompt in [
        format!("{background}\n\nWhat is the Juniper ferry boarding gate?"),
        format!("What is the Juniper ferry boarding gate?\n\n{background}"),
    ] {
        let result = fixture.retrieve(&prompt);
        assert_markers(
            &result,
            &["GATE_EAST_ARCH"],
            &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 1);
    }
}

#[test]
fn separate_questions_can_be_answered_by_separate_records() {
    let fixture = ferry_fixture();
    fixture.add(
        "Maple greenhouse watering interval",
        "The Maple greenhouse watering interval is every third morning. WATER_THIRD_MORNING",
    );
    fixture.add(
        "Maple greenhouse ventilation settings",
        "The Maple greenhouse ventilation settings open the roof panels. VENT_ROOF_PANELS",
    );
    for prompt in [
        "What is the Juniper ferry boarding gate? What is the Maple greenhouse watering interval?",
        "Questions:\n- What is the Juniper ferry boarding gate?\n- What is the Maple greenhouse watering interval?",
        "Question 1: What is the Juniper ferry boarding gate?\nQuestion 2: What is the Maple greenhouse watering interval?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &["GATE_EAST_ARCH", "WATER_THIRD_MORNING"],
            &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP", "VENT_ROOF_PANELS"],
        );
        assert_eq!(result.included_trace_count, 2, "prompt: {prompt}");
    }
}

#[test]
fn body_only_answer_does_not_depend_on_question_word_order() {
    let fixture = Fixture::new();
    fixture.add(
        "Collection ledger",
        "Copper orchard parcel pickup is at the stone shelter. PICKUP_STONE_SHELTER",
    );
    fixture.add(
        "Delivery ledger",
        "Copper orchard compost delivery is at the timber shed. COMPOST_TIMBER_SHED",
    );
    for prompt in [
        "Where is copper orchard parcel pickup?",
        "Copper orchard parcel pickup is where?",
        "Where is parcel pickup for copper orchard?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(&result, &["PICKUP_STONE_SHELTER"], &["COMPOST_TIMBER_SHED"]);
        assert_eq!(result.included_trace_count, 1, "prompt: {prompt}");
    }
}

#[test]
fn question_phrasing_preserves_the_requested_subject() {
    let fixture = ferry_fixture();
    for prompt in [
        "Which recorded location is currently used for the Juniper ferry boarding gate?",
        "For the Juniper ferry boarding gate, which location is recorded?",
        "Could you tell me the Juniper ferry boarding gate?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &["GATE_EAST_ARCH"],
            &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 1, "prompt: {prompt}");
    }
}

#[test]
fn known_identifier_does_not_erase_unknown_compound_constraint() {
    let fixture = Fixture::new();
    fixture.add(
        "NR-42 meadow-73 rollback owner",
        "The NR-42 meadow-73 rollback owner is the release steward. OWNER_RELEASE_STEWARD",
    );
    fixture.add(
        "NR-42 meadow-72 rollback owner",
        "The NR-42 meadow-72 rollback owner is the archive steward. OWNER_ARCHIVE_STEWARD",
    );
    let result = fixture.retrieve("Who is the NR-42 meadow-73 rollback owner?");
    assert_markers(
        &result,
        &["OWNER_RELEASE_STEWARD"],
        &["OWNER_ARCHIVE_STEWARD"],
    );
    assert_eq!(result.included_trace_count, 1);

    let absent = fixture.retrieve("Who is the NR-42 meadow-74 rollback owner?");
    assert_markers(
        &absent,
        &[],
        &["OWNER_RELEASE_STEWARD", "OWNER_ARCHIVE_STEWARD"],
    );
    assert_eq!(absent.included_trace_count, 0);
    assert!(absent.context.is_empty());
}

#[test]
fn current_and_old_status_constraints_do_not_cross_match() {
    let fixture = Fixture::new();
    fixture.add(
        "NR42-current rollback owner",
        "The NR42-current rollback owner is the operations steward. CURRENT_OPERATIONS_STEWARD",
    );
    fixture.add(
        "NR42-old rollback owner",
        "The NR42-old rollback owner was the planning steward. OLD_PLANNING_STEWARD",
    );
    for (prompt, expected, wrong) in [
        (
            "Who is the NR42-current rollback owner?",
            "CURRENT_OPERATIONS_STEWARD",
            "OLD_PLANNING_STEWARD",
        ),
        (
            "Who was the NR42-old rollback owner?",
            "OLD_PLANNING_STEWARD",
            "CURRENT_OPERATIONS_STEWARD",
        ),
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(&result, &[expected], &[wrong]);
        assert_eq!(result.included_trace_count, 1, "prompt: {prompt}");
    }
}

#[test]
fn known_identifier_does_not_erase_a_contradictory_topic() {
    let fixture = Fixture::new();
    fixture.add(
        "NR-42 ferry boarding gate",
        "NR-42 records the ferry boarding gate at the east arch. FERRY_ONLY_RECORD",
    );
    fixture.add(
        "NR-43 orchard irrigation schedule",
        "NR-43 records the orchard irrigation schedule at sunrise. OTHER_ID_IRRIGATION",
    );
    let result = fixture.retrieve("What is the NR-42 orchard irrigation schedule?");
    assert_markers(&result, &[], &["FERRY_ONLY_RECORD", "OTHER_ID_IRRIGATION"]);
    assert_eq!(result.included_trace_count, 0);
    assert!(result.context.is_empty());
}

#[test]
fn exact_identifier_only_request_still_retrieves_its_record() {
    let fixture = Fixture::new();
    fixture.add(
        "NR-42 boarding policy",
        "The boarding policy allows one travel trunk. EXACT_ID_TRAVEL_TRUNK",
    );
    fixture.add(
        "NR-43 baggage policy",
        "The baggage policy allows two hand baskets. OTHER_ID_HAND_BASKETS",
    );
    let result = fixture.retrieve("NR-42");
    assert_markers(
        &result,
        &["EXACT_ID_TRAVEL_TRUNK"],
        &["OTHER_ID_HAND_BASKETS"],
    );
    assert_eq!(result.included_trace_count, 1);
}

#[test]
fn independent_identifiers_can_be_served_by_independent_records() {
    let fixture = Fixture::new();
    fixture.add(
        "NR-42 boarding policy",
        "The boarding policy allows one travel trunk. FIRST_ID_TRAVEL_TRUNK",
    );
    fixture.add(
        "NR-43 baggage policy",
        "The baggage policy allows two hand baskets. SECOND_ID_HAND_BASKETS",
    );
    fixture.add(
        "NR-44 customs policy",
        "The customs policy requires a paper receipt. THIRD_ID_PAPER_RECEIPT",
    );
    let result =
        fixture.retrieve("What is the NR-42 boarding policy? What is the NR-43 baggage policy?");
    assert_markers(
        &result,
        &["FIRST_ID_TRAVEL_TRUNK", "SECOND_ID_HAND_BASKETS"],
        &["THIRD_ID_PAPER_RECEIPT"],
    );
    assert_eq!(result.included_trace_count, 2);
}

#[test]
fn busy_first_question_does_not_starve_the_second_with_a_small_global_cap() {
    let fixture = Fixture::new();
    for index in 0..8 {
        fixture.add(
            &format!("Juniper ferry boarding gate note {index}"),
            &format!("The Juniper ferry boarding gate is the east arch. BUSY_FERRY_ANSWER_{index}"),
        );
    }
    fixture.add(
        "Maple greenhouse watering interval",
        "The Maple greenhouse watering interval is every third morning. SECOND_QUESTION_WATERING",
    );
    let mut options = options();
    options.max_results = 2;
    let result = fixture.retrieve_with(
        "What is the Juniper ferry boarding gate? What is the Maple greenhouse watering interval?",
        options,
    );
    assert_markers(
        &result,
        &["BUSY_FERRY_ANSWER_", "SECOND_QUESTION_WATERING"],
        &[],
    );
    assert_eq!(result.included_trace_count, 2);
    assert!(result.candidate_match_count <= 4);
}

#[test]
fn overlapping_questions_deduplicate_records_and_candidate_counts() {
    let fixture = Fixture::new();
    fixture.add(
        "Juniper ferry boarding gate",
        "The Juniper ferry boarding gate is the east arch. SINGLE_SHARED_ANSWER",
    );
    let result = fixture.retrieve(
        "What is the Juniper ferry boarding gate? Where is the Juniper ferry boarding gate? \
         What is the Juniper ferry boarding gate?",
    );
    assert_markers(&result, &["SINGLE_SHARED_ANSWER"], &[]);
    assert_eq!(result.context.matches("SINGLE_SHARED_ANSWER").count(), 1);
    assert_eq!(result.candidate_match_count, 1);
    assert_eq!(result.search_match_count, 1);
    assert_eq!(result.included_trace_count, 1);
    assert!(result.query_term_count <= 16);
}

#[test]
fn many_matching_records_respect_global_candidate_result_and_character_caps() {
    let fixture = Fixture::new();
    for index in 0..40 {
        fixture.add(
            &format!("Juniper ferry boarding gate note {index}"),
            &format!(
                "The Juniper ferry boarding gate is the east arch. CAPPED_GATE_RECORD_{index:02}"
            ),
        );
    }
    let mut options = options();
    options.max_results = 16;
    options.max_chars = 8_000;
    let result = fixture.retrieve_with("What is the Juniper ferry boarding gate?", options);
    assert_markers(&result, &["CAPPED_GATE_RECORD_"], &[]);
    assert!(result.candidate_match_count <= 32);
    assert!(result.included_trace_count <= 16);
    assert!(result.context.chars().count() <= 8_000);
    assert_eq!(result.context_chars, result.context.chars().count());
    assert_eq!(
        result.context.matches("CAPPED_GATE_RECORD_").count(),
        result.included_trace_count
    );
    for index in 0..40 {
        assert!(
            result
                .context
                .matches(&format!("CAPPED_GATE_RECORD_{index:02}"))
                .count()
                <= 1
        );
    }
}

#[test]
fn absent_question_does_not_include_unrelated_or_near_topic_records() {
    let fixture = ferry_fixture();
    for prompt in [
        "What is the cobalt observatory mirror diameter?",
        "What is the Juniper ferry engine serial?",
        "What is the meadow-74 rollback owner?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &[],
            &["GATE_EAST_ARCH", "SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 0, "prompt: {prompt}");
        assert!(result.context.is_empty(), "prompt: {prompt}");
    }
}

#[test]
fn question_list_labels_are_metadata_not_fact_identifiers() {
    let fixture = Fixture::new();
    fixture.add(
        "Collection ledger",
        "Copper orchard parcel pickup is at the stone shelter. PICKUP_STONE_SHELTER",
    );
    for label in ["delivery-check-91", "handoff_item_024", "17"] {
        let prompt = format!("Questions:\n- {label}: Where is copper orchard parcel pickup?");
        let result = fixture.retrieve(&prompt);
        assert_markers(&result, &["PICKUP_STONE_SHELTER"], &[]);
        assert_eq!(result.included_trace_count, 1);
    }
}

#[test]
fn label_handling_preserves_identifiers_in_questions_and_nonlabel_context() {
    let fixture = ferry_fixture();
    for prompt in [
        "Questions:\n- delivery-check-91: What is the missing-probe-774 Juniper ferry boarding gate?",
        "NR42: What is the Juniper ferry boarding gate?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &[],
            &["GATE_EAST_ARCH", "SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 0, "prompt: {prompt}");
    }
}

#[test]
fn requested_value_framing_anchors_on_the_named_subject() {
    let fixture = ferry_fixture();
    for prompt in [
        "Which recorded setting applies to the Juniper ferry boarding gate?",
        "Which setting currently governs the boarding gate for the Juniper ferry?",
        "Which stored setting governs the boarding gate for the Juniper ferry?",
    ] {
        let result = fixture.retrieve(prompt);
        assert_markers(
            &result,
            &["GATE_EAST_ARCH"],
            &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
        );
        assert_eq!(result.included_trace_count, 1, "prompt: {prompt}");
    }
}

#[test]
fn coordinated_independent_subjects_retrieve_separate_records() {
    let fixture = ferry_fixture();
    fixture.add(
        "Maple greenhouse watering interval",
        "The Maple greenhouse watering interval is every third morning. WATER_THIRD_MORNING",
    );
    let result = fixture.retrieve(
        "Which recorded setting applies to the boarding gate for the Juniper ferry \
         and the watering interval for the Maple greenhouse?",
    );
    assert_markers(
        &result,
        &["GATE_EAST_ARCH", "WATER_THIRD_MORNING"],
        &["SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
    );
    assert_eq!(result.included_trace_count, 2);
}

#[test]
fn requested_value_framing_does_not_replace_specific_missing_descriptors() {
    let fixture = Fixture::new();
    fixture.add(
        "Launch ledger",
        "The launch credential for the September rollout is held by operations. ORDINARY_CREDENTIAL",
    );
    let result = fixture
        .retrieve("Which cerulean launch credential was recorded for the September rollout?");
    assert_markers(&result, &[], &["ORDINARY_CREDENTIAL"]);
    assert_eq!(result.included_trace_count, 0);
}

#[test]
fn compound_scope_is_not_counted_twice_to_admit_missing_evidence() {
    let fixture = Fixture::new();
    fixture.add(
        "project-a shipping notes",
        "project-a shipping is handled by the operations steward. SHIPPING_ONLY",
    );
    let present = fixture.retrieve("What is the project-a shipping?");
    assert_markers(&present, &["SHIPPING_ONLY"], &[]);
    let absent = fixture.retrieve("What is the project-a shipping cerulean credential?");
    assert_markers(&absent, &[], &["SHIPPING_ONLY"]);
    assert_eq!(absent.included_trace_count, 0);
}

#[test]
fn conditional_response_instructions_are_not_independent_questions() {
    let fixture = ferry_fixture();
    fixture.add(
        "When abstain set answer empty string provenance array",
        "When abstain is true, set answer to the empty string and provenance to an empty array. FORMAT_ONLY",
    );
    let result = fixture.retrieve(
        "When abstain=true, set answer to the empty string and provenance to an empty array.\n\
         Questions:\n- travel-17: What is the Juniper ferry boarding gate?",
    );
    assert_markers(
        &result,
        &["GATE_EAST_ARCH"],
        &["FORMAT_ONLY", "SEATING_UPSTAIRS", "RAILWAY_WEST_RAMP"],
    );
    assert_eq!(result.included_trace_count, 1);
}
